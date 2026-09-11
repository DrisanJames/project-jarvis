package contentdesk

// Contract goldens (testdata/contract/*.json) are written from the REAL
// handlers on real Postgres by cmd/server/content_desk_contract_integration_test.go.
// These tests need no DB: every golden must decode STRICTLY into the Go types
// (a renamed or removed field fails here first), must still exercise every
// block type, and the portal's copy must be byte-identical. The publisher's
// copy (another repo) is checked by scripts/check-content-desk-contract.sh.

import (
	"bytes"
	"crypto/sha256"
	"encoding/json"
	"os"
	"path/filepath"
	"sort"
	"testing"
)

const (
	contractDir       = "testdata/contract"
	portalContractDir = "../../web/src/components/mailing/components/__fixtures__/content-desk"
)

func strictDecode(t *testing.T, name string, v any) {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join(contractDir, name))
	if err != nil {
		t.Fatal(err)
	}
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.DisallowUnknownFields()
	if err := dec.Decode(v); err != nil {
		t.Fatalf("%s no longer matches the Go response types: %v", name, err)
	}
}

func TestContractGoldensDecodeIntoBackendTypes(t *testing.T) {
	var sites struct {
		Sites []Site `json:"sites"`
	}
	var briefs struct {
		Briefs []Brief `json:"briefs"`
	}
	var list struct {
		Articles []Article `json:"articles"`
	}
	var detail, withdrawn, released ArticleDetail
	var ops OpsStatus
	var manifest struct {
		Site         Site            `json:"site"`
		SiteID       string          `json:"site_id"`
		Manifest     []ManifestEntry `json:"manifest"`
		ManifestHash string          `json:"manifest_hash"`
	}
	var rel struct {
		Release Release `json:"release"`
	}
	strictDecode(t, "sites.json", &sites)
	strictDecode(t, "briefs.json", &briefs)
	strictDecode(t, "list.json", &list)
	strictDecode(t, "detail.json", &detail)
	strictDecode(t, "detail_withdrawn.json", &withdrawn)
	strictDecode(t, "detail_released.json", &released)
	strictDecode(t, "ops_status.json", &ops)
	strictDecode(t, "manifest.json", &manifest)
	strictDecode(t, "release_create.json", &rel)

	if detail.Revision == nil {
		t.Fatal("detail.json must carry a revision")
	}
	seen := map[string]bool{}
	for _, b := range detail.Revision.Package.Blocks {
		seen[b.Type] = true
	}
	for typ := range AllowedBlockTypes {
		if !seen[typ] {
			t.Errorf("detail.json no longer exercises block type %q — the portal and publisher renderers go untested", typ)
		}
	}
	for _, j := range detail.Revision.Checks.Judgment {
		if j.Sentence == "" {
			t.Errorf("judgment %s has no sentence", j.ID)
		}
	}
	if withdrawn.Article.Status != StatusWithdrawn {
		t.Errorf("detail_withdrawn.json status = %q", withdrawn.Article.Status)
	}
	if len(manifest.Manifest) != 1 || released.Revision == nil || manifest.Manifest[0].ArticleID != released.Article.ID ||
		manifest.Manifest[0].RevisionHash != released.Revision.RevisionHash {
		t.Errorf("detail_released.json must be the manifest's article at the manifest's revision")
	}
	if manifest.Site.Domain == "" || manifest.Site.ID != manifest.SiteID || manifest.ManifestHash != rel.Release.ManifestHash {
		t.Errorf("manifest/release goldens disagree: site=%+v release hash %s", manifest.Site, rel.Release.ManifestHash)
	}
}

func contractSums(t *testing.T, dir string) map[string][32]byte {
	t.Helper()
	files, err := filepath.Glob(filepath.Join(dir, "*.json"))
	if err != nil {
		t.Fatal(err)
	}
	out := map[string][32]byte{}
	for _, f := range files {
		b, err := os.ReadFile(f)
		if err != nil {
			t.Fatal(err)
		}
		out[filepath.Base(f)] = sha256.Sum256(b)
	}
	return out
}

func TestContractGoldensMirroredInPortal(t *testing.T) {
	want, got := contractSums(t, contractDir), contractSums(t, portalContractDir)
	if len(want) == 0 {
		t.Fatal("no contract goldens")
	}
	var names []string
	for n := range want {
		names = append(names, n)
	}
	for n := range got {
		if _, ok := want[n]; !ok {
			names = append(names, n)
		}
	}
	sort.Strings(names)
	for _, n := range names {
		if want[n] != got[n] {
			t.Errorf("%s: portal fixture drifted from the backend golden (scripts/check-content-desk-contract.sh --sync)", n)
		}
	}
}
