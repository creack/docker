package multio

import (
	"encoding/binary"
	"errors"
	"io"
	"log"
)

const (
	PageSize = 32 * 1024

	Version = 1
)

// Message padding
const (
	HeaderLen = 12 // Size of the header

	VersionIndex = iota << 2
	IDIndex      // Index where is the Fd
	SizeIndex    // Index where is the Frame size
	TypeIndex    // Index where is the type of the frame
)

// Message types
const (
	WriteRequest uint8 = iota
	ReadRequest
	AckWrite
	AckRead
	AckNotReady
)

// New message types
const (
	Frame = iota
	Ack
	Close
)

var (
	ErrWrongReqSize      = errors.New("Error reading the request: wrong size")
	ErrUnkownRequestType = errors.New("Unkown request type or invalid request")
	ErrWrongType         = errors.New("Multiplexer need to have a Writer and a Reader as argument")

	ErrInvalidStdHeader = errors.New("Unrecognized input header") // FIXE: remove?
)

/*
	id := binary.BigEndian.Uint32(buf[IDIndex : IDIndex+4])
	binary.BigEndian.PutUint32(ack[IDIndex:IDIndex+4], uint32(id))
*/

type reqMap map[uint32]chan []byte

type msg struct {
	data []byte
	n    int
	err  error
}

type msgMap map[uint32]chan msg

type Multiplexer struct {
	r         io.Reader
	w         io.Writer
	c         io.Closer // TODO: implement Close()
	msgMap    msgMap
	writeChan chan []byte
	readChans map[int]chan *Message
	ackChans  map[int]chan *Message

	writeReq reqMap
	readReq  reqMap
}

// decode cannot fail. In case of error, it populate the field err from Message.
func (m *Multiplexer) decodeMsg(src []byte, err error) *Message {
	msg := &Message{}
	msg.decode(src, err)
	return msg
}

func (m *Multiplexer) encodeMsg(src []byte) []byte {
	msg := &Message{
		data: src,
	}
	msg.encode()
	return msg.encode()
}

func (m *Multiplexer) StartRead() error {
	buf := make([]byte, PageSize+HeaderLen)
	for {
		n, err := m.r.Read(buf)
		msg := m.decodeMsg(buf[:n], err)
		switch msg.kind {
		case Frame:
			go func() {
				m.readChans[int(msg.id)] <- msg
			}()
		case Ack:
			m.ackChans[int(msg.id)] <- msg
		case Close:
			panic("unimplemented")
		}
	}
	return nil
}

func (m *Multiplexer) StartWrite() error {
	for buf := range m.writeChan {
		m.w.Write(m.encodeMsg(buf))
	}
	return nil
}

func NewMultiplexer(rwc interface{}) (*Multiplexer, error) {
	m := &Multiplexer{}
	if r, ok := rwc.(io.Reader); ok {
		m.r = r
	}
	if w, ok := rwc.(io.Writer); ok {
		m.w = w
	}
	if c, ok := rwc.(io.Closer); ok {
		m.c = c
	}
	if m.r == nil || m.w == nil {
		return nil, ErrWrongType
	}
	m.writeReq = reqMap{}
	m.readReq = reqMap{}
	go m.StartRead()
	go m.StartWrite()
	return m, nil
}

func (m *Multiplexer) NewWriter(id int) io.Writer {
	if _, exists := m.ackChans[id]; exists {
		return nil
	}

	m.ackChans[id] = make(chan *Message)

	return &Writer{
		writeChan: m.writeChan,
		ackChan:   m.ackChans[id],
	}
}

func (m *Multiplexer) NewReader(id int) io.Reader {
	return &Reader{}
}

type Header [HeaderLen]byte

type Writer struct {
	writeChan chan []byte
	ackChan   chan *Message
}

func (w *Writer) Write(buf []byte) (n int, err error) {
	w.writeChan <- buf
	msg := <-w.ackChan
	return msg.n, msg.err
}

type Reader struct {
	readChan chan *Message
	ackChan  chan *Message
}

// Blocks until the full page has been read
func (r *Reader) Read(buf []byte) (int, error) {
	// Wait for a message
	msg := <-r.readChan
	copy(buf, msg.data)

	// Send ACK
	r.ackChan <- msg

	return msg.n, msg.err
}
