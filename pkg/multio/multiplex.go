package multio

import (
	"encoding/binary"
	"errors"
	"io"
)

const (
	HeaderLen = 8 // Size of the header
	IDIndex   = 0 // Index where is the Fd
	SizeIndex = 4 // Index where is the Frame size
)

var (
	ErrWrongType        = errors.New("Multiplexer need to have a Writer and a Reader as argument")
	ErrInvalidStdHeader = errors.New("Unrecognized input header")
)

type Multiplexer struct {
	r io.Reader
	w io.Writer
	c io.Closer // TODO: implement Close()
}

func (m *Multiplexer) Start() error {
	var (
		buf = make([]byte, 32*1024)
	)
	for {
		// Read ACK with ID
		n, err := m.r.Read(buf)

		// Read page

		// Send it to the chan
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
	go m.Start()
	return m, nil
}

func (m *Multiplexer) NewWriter(id uint8) io.Writer {
	return &Writer{
		Writer:  m.w,
		prefix:  StdType{0: id},
		sizeBuf: make([]byte, 4), // We encode the size in 32bits.
	}
}

func (m *Multiplexer) NewReader(id int) io.Reader {
	return &Reader{
		Reader:  m.r,
		w:       m.w,
		id:      id,
		sizeBuf: make([]byte, 4),       // We encode the size in 32 bits
		dataBuf: make([]byte, 32*1024), // Allocate a page so we don't need to worry about it.
	}
}

type StdType [HeaderLen]byte
type Header [HeaderLen]byte

var (
	Stdin  StdType = StdType{0: 0}
	Stdout StdType = StdType{0: 1}
	Stderr StdType = StdType{0: 2}
)

// Writer is multiplexed Writer.
// Everything written to it will be encapsulated using a custom format,
// and written to the underlying `w` stream.
// This allows multiple write streams (e.g. stdout and stderr) to be muxed into a single connection.
// `t` indicates the id of the stream to encapsulate.
// It can be utils.Stdin, utils.Stdout, utils.Stderr.
type Writer struct {
	io.Writer
	prefix  StdType
	sizeBuf []byte
}

func (w *Writer) Write(buf []byte) (n int, err error) {
	if w == nil || w.Writer == nil {
		return 0, errors.New("Writer not instanciated")
	}
	binary.BigEndian.PutUint32(w.prefix[4:], uint32(len(buf)))
	buf = append(w.prefix[:], buf...)

	n, err = w.Writer.Write(buf)
	return n - HeaderLen, err
}

type Reader struct {
	io.Reader
	id      int
	w       io.Writer
	prefex  StdType
	sizeBuf []byte
	dataBuf []byte
}

// Blocks until the full page has been read
func (r *Reader) Read(buf []byte) (int, error) {
	if r == nil || r.w == nil {
		return 0, errors.New("not instanciated")
	}

	// Ask the other side to write
	header := make([]byte, HeaderLen)
	// put the id and the size in the header
	binary.BigEndian.PutUint32(header[IDIndex:IDIndex+4], uint32(r.id))
	binary.BigEndian.PutUint32(header[SizeIndex:SizeIndex+4], uint32(len(buf)))
	// Send the header
	n, err := r.w.Write(header)
	if n != HeaderLen {
		return n, io.ErrShortWrite
	}

	// Block until acknowledgement

	// Actually read
	var (
		nr int
	)
	for nr < HeaderLen {
		nr2, er := r.Reader.Read(r.dataBuf[nr:])
		if er != nil {
			return nr + nr2, er
		}
		nr += nr2
	}
	return 0, nil
}

// StdCopy is a modified version of io.Copy.
//
// StdCopy will demultiplex `src`, assuming that it contains two streams,
// previously multiplexed together using a StdWriter instance.
// As it reads from `src`, StdCopy will write to `dstout` and `dsterr`.
//
// StdCopy will read until it hits EOF on `src`. It will then return a nil error.
// In other words: if `err` is non nil, it indicates a real underlying error.
//
// `written` will hold the total number of bytes written to `dstout` and `dsterr`.
func StdCopy(dstout, dsterr io.Writer, src io.Reader) (written int64, err error) {
	var (
		buf       = make([]byte, 32*1024+HeaderLen+1)
		bufLen    = len(buf)
		nr, nw    int
		er, ew    error
		out       io.Writer
		frameSize int
	)

	for {
		// Make sure we have at least a full header
		for nr < HeaderLen {
			var nr2 int
			nr2, er = src.Read(buf[nr:])
			if er == io.EOF {
				return written, nil
			}
			if er != nil {
				return 0, er
			}
			nr += nr2
		}

		// Check the first byte to know where to write
		switch buf[FdIndex] {
		case 0:
			fallthrough
		case 1:
			// Write on stdout
			out = dstout
		case 2:
			// Write on stderr
			out = dsterr
		default:
			// FIXME: use os.NewFile() on the custom fd
			return 0, ErrInvalidStdHeader
		}

		// Retrieve the size of the frame
		frameSize = int(binary.BigEndian.Uint32(buf[SizeIndex : SizeIndex+4]))

		// Check if the buffer is big enough to read the frame.
		// Extend it if necessary.
		if frameSize+HeaderLen > bufLen {
			buf = append(buf, make([]byte, frameSize-len(buf)+1)...)
			bufLen = len(buf)
		}

		// While the amount of bytes read is less than the size of the frame + header, we keep reading
		for nr < frameSize+HeaderLen {
			var nr2 int
			nr2, er = src.Read(buf[nr:])
			if er == io.EOF {
				return written, nil
			}
			if er != nil {
				return 0, er
			}
			nr += nr2
		}

		// Write the retrieved frame (without header)
		nw, ew = out.Write(buf[HeaderLen : frameSize+HeaderLen])
		if nw > 0 {
			written += int64(nw)
		}
		if ew != nil {
			return 0, ew
		}
		// If the frame has not been fully written: error
		if nw != frameSize {
			return 0, io.ErrShortWrite
		}

		// Move the rest of the buffer to the beginning
		copy(buf, buf[frameSize+HeaderLen:])
		// Move the index
		nr -= frameSize + HeaderLen
	}
}
