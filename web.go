package main

import (
	"bytes"
	_ "embed"
	"html/template"
)

//go:embed web/upload.html
var uploadHTMLSource string

var uploadTemplate = template.Must(template.New("upload").Parse(uploadHTMLSource))

// renderUploadPage fills in the drop-zone page's target identity. Uses
// html/template (not text/template) so the identity string - which comes
// from the OS hostname/username and isn't something we fully control - is
// escaped automatically rather than trusted verbatim.
func renderUploadPage(identity string) ([]byte, error) {
	var buf bytes.Buffer
	if err := uploadTemplate.Execute(&buf, struct{ Identity string }{Identity: identity}); err != nil {
		return nil, err
	}
	return buf.Bytes(), nil
}
