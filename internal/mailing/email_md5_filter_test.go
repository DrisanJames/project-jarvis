package mailing

import "testing"

// Pins {{ email | email_md5 }} — the {{token}} in the v8/v9 money links.
func TestEmailMD5Filter(t *testing.T) {
	ts := NewTemplateService()
	out, err := ts.Render("", `{{ email | email_md5 }}`,
		map[string]interface{}{"email": "  Test@Example.COM "})
	if err != nil {
		t.Fatal(err)
	}
	if out != "55502f40dc8b7c769880b10874abc9d0" { // md5("test@example.com")
		t.Errorf("got %q", out)
	}
	out, _ = ts.Render("", `{{ email | email_md5 }}`, map[string]interface{}{"email": ""})
	if out != "" {
		t.Errorf("empty email should render empty, got %q", out)
	}
}
