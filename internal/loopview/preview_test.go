package loopview

import (
	"strings"
	"testing"
)

func TestStaticPreviewRejectsActiveDocuments(t *testing.T) {
	for _, source := range []string{
		`<script>document.body.innerHTML="drawing"</script>`,
		`<svg onload="alert(1)"></svg>`, `<a href="/api/control">go</a>`,
		`<meta http-equiv="refresh" content="0;url=/api/control">`,
		`<iframe srcdoc="hello"></iframe>`, `<svg><foreignObject><p>x</p></foreignObject></svg>`,
		`<svg><animate attributeName="href" values="https://evil.test"/></svg>`,
		`<svg><use href="#x"/></svg>`, `<img src="https://evil.test">`,
		`<form action="/api/control"></form>`, strings.Repeat("x", MaxPreviewBytes+1),
	} {
		if _, err := StaticPreview(source); err == nil {
			t.Fatalf("active/oversized document accepted: %.100s", source)
		}
	}
}

func TestStaticPreviewPreservesDrawingAndRestrictsCSSResources(t *testing.T) {
	result, err := StaticPreview(`<html><head><style>svg{fill:#eee;background:url(https://evil.test/pixel)}</style></head><body><svg viewBox="0 0 300 200"><rect width="50" height="30"/><text x="10" y="60">S1 = 24</text></svg></body></html>`)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(result, `S1 = 24`) || !strings.Contains(result, `viewBox="0 0 300 200"`) || !strings.Contains(result, "default-src &#39;none&#39;") || strings.Index(result, "Content-Security-Policy") > strings.Index(result, "<style>") {
		t.Fatalf("preview lost drawing or first policy: %s", result)
	}
}
