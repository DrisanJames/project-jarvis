package worker

import (
	"crypto/sha256"
	"strings"
	"testing"

	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

const sampleHTML = `<!DOCTYPE html>
<html><head><title>Test</title></head>
<body>
<div style="padding:20px; background-color:#ffffff; font-family:Arial, sans-serif;">
  <table width="600" style="margin:0 auto; border-collapse:collapse;">
    <tr>
      <td style="padding:10px; color:#333333; font-size:16px;">
        <h1>Welcome to Discount Blog</h1>
        <p>Save big on everyday essentials.</p>
      </td>
    </tr>
    <tr>
      <td style="padding:10px;">
        <a href="https://discountblog.com/deals" style="color:#667eea; text-decoration:none;">Check out the latest deals</a>
      </td>
    </tr>
  </table>
</div>
</body>
</html>`

func hash(s string) [32]byte { return sha256.Sum256([]byte(s)) }

func TestMutateHTMLHash_Deterministic(t *testing.T) {
	seed := int64(42)
	out1 := mutateHTMLHash(sampleHTML, seed)
	out2 := mutateHTMLHash(sampleHTML, seed)
	assert.Equal(t, out1, out2, "same seed must produce identical output")
}

func TestMutateHTMLHash_DifferentSeeds(t *testing.T) {
	out1 := mutateHTMLHash(sampleHTML, 1)
	out2 := mutateHTMLHash(sampleHTML, 2)
	out3 := mutateHTMLHash(sampleHTML, 999)

	h1 := hash(out1)
	h2 := hash(out2)
	h3 := hash(out3)

	assert.NotEqual(t, h1, h2, "different seeds should produce different hashes")
	assert.NotEqual(t, h2, h3)
	assert.NotEqual(t, h1, h3)
}

func TestMutateHTMLHash_PreservesVisibleContent(t *testing.T) {
	mutated := mutateHTMLHash(sampleHTML, 12345)

	assert.Contains(t, mutated, "Welcome to Discount Blog")
	assert.Contains(t, mutated, "Save big on everyday essentials.")
	assert.Contains(t, mutated, "Check out the latest deals")
	assert.Contains(t, mutated, "https://discountblog.com/deals")
}

func TestMutateHTMLHash_InjectsComments(t *testing.T) {
	mutated := mutateHTMLHash(sampleHTML, 77)
	assert.Contains(t, mutated, "<!--")
	assert.NotContains(t, sampleHTML, "<!--")
}

func TestMutateHTMLHash_InjectsDataAttributes(t *testing.T) {
	found := false
	for seed := int64(0); seed < 50; seed++ {
		if strings.Contains(mutateHTMLHash(sampleHTML, seed), "data-m=") {
			found = true
			break
		}
	}
	assert.True(t, found, "at least one of 50 seeds should inject a data-m attribute")
}

func TestMutateSubjectLine_Deterministic(t *testing.T) {
	subject := "Save 40% at Target This Weekend"
	s1 := mutateSubjectLine(subject, 100, "discountblog")
	s2 := mutateSubjectLine(subject, 100, "discountblog")
	assert.Equal(t, s1, s2)
}

func TestMutateSubjectLine_DifferentSeeds(t *testing.T) {
	subject := "Save big on the best new deals this week"
	unique := make(map[string]bool)
	for seed := int64(0); seed < 100; seed++ {
		unique[mutateSubjectLine(subject, seed, "discountblog")] = true
	}
	assert.Greater(t, len(unique), 1, "mutations should produce at least 2 distinct subjects across 100 seeds")
}

func TestMutateSubjectLine_PreservesMeaning(t *testing.T) {
	subject := "Top Tips for Easy Savings"
	mutated := mutateSubjectLine(subject, 42, "discountblog")
	lower := strings.ToLower(mutated)

	hasSavings := strings.Contains(lower, "saving")
	hasTips := strings.Contains(lower, "tips") || strings.Contains(lower, "tricks")
	assert.True(t, hasSavings || hasTips, "mutated subject %q should retain core meaning tokens", mutated)
}

func TestMutateSubjectLine_PreservesLiquidTags(t *testing.T) {
	cases := []struct {
		name    string
		subject string
		tag     string
	}{
		{
			name:    "first_name with default",
			subject: "{{ first_name | Default: 'Hey' }}, spring offers land this weekend!",
			tag:     "{{ first_name | Default: 'Hey' }}",
		},
		{
			name:    "email tag",
			subject: "New deals for {{ email }}",
			tag:     "{{ email }}",
		},
		{
			name:    "multiple tags",
			subject: "{{ first_name }}, check out {{ offer_name }} today",
			tag:     "{{ first_name }}",
		},
		{
			name:    "system unsubscribe",
			subject: "Great tips inside {{ system.unsubscribe_url }}",
			tag:     "{{ system.unsubscribe_url }}",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			for seed := int64(0); seed < 50; seed++ {
				result := mutateSubjectLine(tc.subject, seed, "discountblog")
				assert.Contains(t, result, tc.tag,
					"seed %d: Liquid tag %q must survive mutation intact, got: %q", seed, tc.tag, result)
			}
		})
	}
}

func TestMutatePreheader_PreservesLiquidTags(t *testing.T) {
	preheader := "{{ first_name | Default: 'Friend' }}, find the best new deals"
	for seed := int64(0); seed < 50; seed++ {
		result := mutatePreheader(preheader, seed)
		assert.Contains(t, result, "{{ first_name | Default: 'Friend' }}",
			"seed %d: Liquid tag must survive, got: %q", seed, result)
	}
}

func TestComputeMutationSeed(t *testing.T) {
	id1 := uuid.MustParse("11111111-1111-1111-1111-111111111111")
	id2 := uuid.MustParse("22222222-2222-2222-2222-222222222222")
	wave := "wave-abc"

	s1 := computeMutationSeed(id1, wave)
	s2 := computeMutationSeed(id2, wave)
	s3 := computeMutationSeed(id1, wave)

	assert.Equal(t, s1, s3, "same inputs must produce same seed")
	assert.NotEqual(t, s1, s2, "different subscriber IDs must produce different seeds")
}

func TestInjectHoneypotLink(t *testing.T) {
	subID := "aaaaaaaa-bbbb-cccc-dddd-eeeeeeeeeeee"
	result := injectHoneypotLink(sampleHTML, subID)

	assert.Contains(t, result, "api/mailing/bt/")
	assert.Contains(t, result, `aria-hidden="true"`)
	assert.Contains(t, result, `opacity:0`)

	// Must have two-segment format: /api/mailing/bt/{token}/{nonce}
	verifyIdx := strings.Index(result, "api/mailing/bt/")
	require.Greater(t, verifyIdx, 0)
	afterVerify := result[verifyIdx+len("api/mailing/bt/"):]
	quoteIdx := strings.Index(afterVerify, `"`)
	require.Greater(t, quoteIdx, 0)
	pathPart := afterVerify[:quoteIdx]
	assert.Contains(t, pathPart, "/", "honeypot URL must have two segments (token/nonce)")

	bodyCloseIdx := strings.LastIndex(result, "</body>")
	assert.Less(t, verifyIdx, bodyCloseIdx, "honeypot link should be before </body>")
}

func TestInjectHoneypotLink_NoBody(t *testing.T) {
	plain := `<div><p>Hello</p></div>`
	result := injectHoneypotLink(plain, "00000000-0000-0000-0000-000000000000")
	assert.Contains(t, result, "api/mailing/bt/")
}

func TestMutatePreheader(t *testing.T) {
	p := "Check out today's deals and save big"
	unique := make(map[string]bool)
	for seed := int64(0); seed < 50; seed++ {
		unique[mutatePreheader(p, seed)] = true
	}
	assert.Greater(t, len(unique), 1, "preheader mutations should produce distinct outputs")
}

func TestFullPipeline_UniquePerRecipient(t *testing.T) {
	hashes := make(map[[32]byte]bool)
	wave := "test-wave-id"
	subject := "Best deals this week"

	for i := 0; i < 200; i++ {
		subID := uuid.New()
		seed := computeMutationSeed(subID, wave)
		html := mutateHTMLHash(sampleHTML, seed)
		html = injectHoneypotLink(html, subID.String())
		subj := mutateSubjectLine(subject, seed, "discountblog")

		combined := html + "||" + subj
		h := hash(combined)
		assert.False(t, hashes[h], "recipient %d produced duplicate hash", i)
		hashes[h] = true
	}
}
