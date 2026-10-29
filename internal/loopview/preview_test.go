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
		`<svg><desc><script>alert(1)</script></desc></svg>`,
		`<form action="/api/control"></form>`, strings.Repeat("x", MaxPreviewBytes+1),
	} {
		if _, err := StaticPreview(source); err == nil {
			t.Fatalf("active/oversized document accepted: %.100s", source)
		}
	}
}

func TestStaticPreviewPreservesAccessibleSVGDescription(t *testing.T) {
	result, err := StaticPreview(`<svg role="img" aria-labelledby="title description"><title id="title">Card geometry</title><desc id="description">Measured container and three regions.</desc><rect width="384" height="246"/></svg>`)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(result, `aria-labelledby="title description"`) || !strings.Contains(result, `<desc id="description">Measured container and three regions.</desc>`) {
		t.Fatalf("preview lost accessible drawing description: %s", result)
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
