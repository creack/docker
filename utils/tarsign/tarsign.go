package tarsign

import (
	"bufio"
	"bytes"
	"code.google.com/p/go.crypto/openpgp"
	"errors"
	"fmt"
	"github.com/dotcloud/docker/term"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync"
)

var (
	ErrWriteOnClose = errors.New("Write on closed hash")
	ErrEncryptedKey = errors.New("The private key is encrypted. Please provide the password.")
)

type PGPSign struct {
	sync.RWMutex
	r          io.ReadCloser
	w          io.WriteCloser
	closed     bool
	err        error
	promptFct  openpgp.PromptFunction
	result     chan []byte
	gpgKey     string
	privateKey *openpgp.Entity
}

func (ps *PGPSign) startSigner() error {
	println("startSigner")

	ps.RLock()
	if ps.closed {
		ps.RUnlock()
		println("---44------>")
		return ErrWriteOnClose
	}
	ps.RUnlock()

	signature, err := ps.ArmoredSign(ps.r)
	println("ARMOREDSIGN FINISHED")
	if err != nil {
		println("ERROR:", err.Error())
		ps.Lock()
		if !ps.closed {
			println("closing chan")
			close(ps.result)
			println("chan closed")
		}
		ps.err = err
		ps.closed = true
		ps.Unlock()
		return err
	}

	println("vvvvvvvvv")
	ps.result <- signature
	println("^^^^^^^^^")
	return nil
}

func New(gpgKey string, promptFct openpgp.PromptFunction) (*PGPSign, error) {
	if gpgKey == "" {
		gpgKey = os.Getenv("GPGKEY")
		if gpgKey == "" {
			return nil, fmt.Errorf("Missing gpgKey")
		}
	}

	if promptFct == nil {
		promptFct = DefaultPromptFct
	}

	privateKey, err := EntityFromSecring(gpgKey, "")
	if err != nil {
		return nil, err
	}

	h := &PGPSign{
		gpgKey:     gpgKey,
		promptFct:  promptFct,
		privateKey: privateKey,
	}
	h.r, h.w = io.Pipe()
	h.result = make(chan []byte)
	go h.startSigner()
	return h, nil
}

func (ps *PGPSign) Write(buf []byte) (int, error) {
	if ps.err != nil {
		return 0, ps.err
	}
	if ps.closed {
		return 0, ErrWriteOnClose
	}
	return ps.w.Write(buf)
}

func (ps *PGPSign) Sum(extra []byte) []byte {
	if extra != nil {
		if _, err := ps.w.Write(extra); err != nil {
			return nil
		}
	}
	ps.w.Close()
	ps.r.Close()
	ps.closed = true

	return <-ps.result
}

func (ps *PGPSign) Reset() {
	ps.w.Close()
	ps.r.Close()

	ps.closed = false
	ps.err = nil
	ps.r, ps.w = io.Pipe()

	go ps.startSigner()
}

func (*PGPSign) Size() int {
	return 0
}

func (*PGPSign) BlockSize() int {
	return 0
}

func DefaultSecRingPath() string {
	return filepath.Join(os.Getenv("HOME"), ".gnupg", "secring.gpg")
}

// keyFile defaults to $HOME/.gnupg/secring.gpg
func EntityFromSecring(keyId, keyFile string) (*openpgp.Entity, error) {
	keyId = strings.ToUpper(keyId)
	if keyFile == "" {
		keyFile = DefaultSecRingPath()
	}

	secring, err := os.Open(keyFile)
	if err != nil {
		return nil, fmt.Errorf("jsonsign: failed to open keyring: %v", err)
	}
	defer secring.Close()

	el, err := openpgp.ReadKeyRing(secring)
	if err != nil {
		return nil, fmt.Errorf("openpgp.ReadKeyRing of %q: %v", keyFile, err)
	}

	var entity *openpgp.Entity
	for _, e := range el {
		pk := e.PrivateKey
		if pk == nil || (pk.KeyIdString() != keyId && pk.KeyIdShortString() != keyId) {
			continue
		}
		entity = e
	}

	if entity == nil {
		found := []string{}
		for _, e := range el {
			pk := e.PrivateKey
			if pk == nil {
				continue
			}
			found = append(found, pk.KeyIdShortString())
		}
		return nil, fmt.Errorf("didn't find a key in %q for keyId %q; other keyIds in file = %v", keyFile, keyId, found)
	}
	return entity, nil
}

func DefaultPromptFct(keys []openpgp.Key, symetric bool) ([]byte, error) {
	print("Please enter passkey: ")
	state, err := term.SaveState(os.Stdin.Fd())
	if err != nil {
		return nil, err
	}
	if err := term.DisableEcho(os.Stdin.Fd(), state); err != nil {
		return nil, err
	}
	defer term.RestoreTerminal(os.Stdin.Fd(), state)

	code, _, err := bufio.NewReader(os.Stdin).ReadLine()
	if err != nil {
		return nil, err
	}
	term.RestoreTerminal(os.Stdin.Fd(), state)
	println()

	return code, nil
}

func (ps *PGPSign) ArmoredSign(src io.Reader) ([]byte, error) {
	if ps.privateKey.PrivateKey.Encrypted {
		code, err := ps.promptFct([]openpgp.Key{
			openpgp.Key{
				Entity:     ps.privateKey,
				PublicKey:  ps.privateKey.PrimaryKey,
				PrivateKey: ps.privateKey.PrivateKey,
			},
		}, false)
		if err != nil {
			return nil, err
		}
		if err := ps.privateKey.PrivateKey.Decrypt(code); err != nil {
			return nil, err
		}
	}

	var sign bytes.Buffer
	if err := openpgp.ArmoredDetachSign(&sign, ps.privateKey, src, nil); err != nil {
		return nil, err
	}

	return sign.Bytes(), nil
}
