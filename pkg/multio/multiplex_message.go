package multio

type Message struct {
	version uint32
	kind    uint32
	id      uint32
	size    uint32

	data []byte
	n    int
	err  error

	ack chan struct{}
}

// decode can't fail. If populate the err field in case of error
func (m *Message) decode(src []byte, err error) {
	m.n = len(src)
	m.err = err
}

// encode cannot fail
func (m *Message) encode() []byte {
	return nil
}

func NewMessage() *Message {
	return &Message{}
}
