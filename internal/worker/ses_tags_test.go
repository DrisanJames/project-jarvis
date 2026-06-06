package worker

import "testing"

func TestSanitizeSESTagValue(t *testing.T) {
	cases := []struct{ in, want string }{
		{"550e8400-e29b-41d4-a716-446655440000", "550e8400-e29b-41d4-a716-446655440000"},
		{"discountblog.com", "discountblog_com"},
		{"gmail", "gmail"},
		{"a b@c", "a_b_c"},
		{"", "none"},
	}
	for _, c := range cases {
		if got := sanitizeSESTagValue(c.in); got != c.want {
			t.Fatalf("sanitizeSESTagValue(%q) = %q, want %q", c.in, got, c.want)
		}
	}
}

func TestSanitizeSESTagValue_LengthCap(t *testing.T) {
	long := make([]byte, 400)
	for i := range long {
		long[i] = 'a'
	}
	got := sanitizeSESTagValue(string(long))
	if len(got) != 256 {
		t.Fatalf("expected value capped at 256, got %d", len(got))
	}
}

func TestBuildSESMessageTags(t *testing.T) {
	got := buildSESMessageTags("send-1", "camp-1", "sub-1", "gmail")
	want := "recipient_send_id=send-1, campaign_id=camp-1, subscriber_id=sub-1, isp_group=gmail, route_type=ses_tenant"
	if got != want {
		t.Fatalf("buildSESMessageTags mismatch:\n got=%q\nwant=%q", got, want)
	}
}
