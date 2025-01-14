package message

import (
	"bufio"
	"encoding/json"

	"go.gazette.dev/core/labels"
)

// JSONFraming is a Framing implementation which encodes messages as line-
// delimited JSON. Messages must be encode-able by the encoding/json package.
var JSONFraming = new(jsonFraming)

type jsonFraming struct{}

// ContentType returns labels.ContentType_JSONLines.
func (*jsonFraming) ContentType() string { return labels.ContentType_JSONLines }

// Marshal implements Framing.
func (*jsonFraming) Marshal(msg Message, bw *bufio.Writer) error {
	return json.NewEncoder(bw).Encode(msg)
}

// Unpack implements Framing.
func (*jsonFraming) Unpack(r *bufio.Reader) ([]byte, error) {
	return UnpackLine(r)
}

// Unmarshal implements Framing.
func (*jsonFraming) Unmarshal(line []byte, msg Message) error {
	if err := json.Unmarshal(line, msg); err != nil {
		return err
	} else if f, ok := msg.(Fixupable); ok {
		return f.Fixup()
	}
	return nil
}
