package loopview

import (
	"fmt"
	"strings"

	"golang.org/x/net/html"
)

const MaxPreviewBytes = 2 * 1024 * 1024

// The iframe also has an empty sandbox attribute. This policy confines CSS
// resources; the structural allowlist rejects navigation and active content.
const PreviewCSP = "default-src 'none'; script-src 'none'; style-src 'unsafe-inline'; img-src 'none'; font-src 'none'; connect-src 'none'; frame-src 'none'; object-src 'none'; base-uri 'none'; form-action 'none'"

// StaticPreview returns a display derivative, never an acceptance observation.
// Dynamic drawings must be serialized to static SVG by their producer first.
func StaticPreview(source string) (string, error) {
	if len(source) > MaxPreviewBytes {
		return "", fmt.Errorf("HTML preview exceeds %d bytes", MaxPreviewBytes)
	}
	doc, err := html.Parse(strings.NewReader(source))
	if err != nil {
		return "", fmt.Errorf("HTML preview parse failed: %w", err)
	}
	allowed := " html head body title meta style div span p br hr section article main header footer aside h1 h2 h3 h4 h5 h6 ul ol li dl dt dd table thead tbody tfoot tr th td caption colgroup col pre code b strong i em small sub sup blockquote figure figcaption svg g defs path rect circle ellipse line polyline polygon text tspan marker pattern clippath mask lineargradient radialgradient stop "
	var head *html.Node
	count := 0
	var visit func(*html.Node) error
	visit = func(n *html.Node) error {
		count++
		if count > 20000 {
			return fmt.Errorf("HTML preview exceeds element bound")
		}
		if n.Type == html.ElementNode {
			name := strings.ToLower(n.Data)
			if !strings.Contains(allowed, " "+name+" ") {
				return fmt.Errorf("HTML preview contains unsupported element %q; deliver static HTML/SVG without scripts, links or embedded resources", name)
			}
			if name == "head" {
				head = n
			}
			for _, a := range n.Attr {
				key := strings.ToLower(a.Key)
				if strings.HasPrefix(key, "on") || key == "href" || key == "src" || key == "srcset" || key == "http-equiv" || key == "action" || key == "formaction" || key == "ping" || key == "srcdoc" || key == "is" {
					return fmt.Errorf("HTML preview contains unsupported attribute %q", key)
				}
			}
		}
		for child := n.FirstChild; child != nil; child = child.NextSibling {
			if err := visit(child); err != nil {
				return err
			}
		}
		return nil
	}
	if err := visit(doc); err != nil {
		return "", err
	}
	if head == nil {
		return "", fmt.Errorf("HTML preview head missing")
	}
	head.InsertBefore(&html.Node{Type: html.ElementNode, Data: "meta", Attr: []html.Attribute{{Key: "http-equiv", Val: "Content-Security-Policy"}, {Key: "content", Val: PreviewCSP}}}, head.FirstChild)
	var out strings.Builder
	if err := html.Render(&out, doc); err != nil {
		return "", fmt.Errorf("HTML preview serialization failed: %w", err)
	}
	if out.Len() > MaxPreviewBytes {
		return "", fmt.Errorf("HTML preview derivative exceeds byte bound")
	}
	return out.String(), nil
}
