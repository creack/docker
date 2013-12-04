package tarsign

import (
	"bufio"
	"bytes"
	"code.google.com/p/go.crypto/openpgp"
	"fmt"
	"github.com/dotcloud/docker/term"
	"github.com/dotcloud/docker/utils/tarsum"
	"io"
	"os"
	"path/filepath"
	"strings"
)

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
	state, err := term.SaveState(0)
	if err != nil {
		return nil, err
	}
	if err := term.DisableEcho(0, state); err != nil {
		return nil, err
	}
	defer term.RestoreTerminal(0, state)

	code, _, err := bufio.NewReader(os.Stdin).ReadLine()
	if err != nil {
		return nil, err
	}
	term.RestoreTerminal(0, state)
	println()

	return code, nil
}

func ArmoredSign(src io.Reader, gpgKey string, promptFct openpgp.PromptFunction) ([]byte, error) {
	if gpgKey == "" {
		gpgKey = os.Getenv("GPGKEY")
		if gpgKey == "" {
			return nil, fmt.Errorf("Missing gpgKey")
		}
	}

	privateKey, err := EntityFromSecring(gpgKey, "")
	if err != nil {
		return nil, err
	}

	if privateKey.PrivateKey.Encrypted {
		if promptFct == nil {
			promptFct = DefaultPromptFct
		}

		code, err := promptFct([]openpgp.Key{
			openpgp.Key{
				Entity:     privateKey,
				PublicKey:  privateKey.PrimaryKey,
				PrivateKey: privateKey.PrivateKey,
			},
		}, false)
		if err != nil {
			return nil, err
		}
		if err := privateKey.PrivateKey.Decrypt(code); err != nil {
			return nil, err
		}
	}

	var sign bytes.Buffer
	if err := openpgp.ArmoredDetachSign(&sign, privateKey, src, nil); err != nil {
		return nil, err
	}

	return sign.Bytes(), nil
}

func TarSign(src io.Reader) error {
	src = &tarsum.TarSum{Reader: src}
	if src == nil {
		return fmt.Errorf("No source")
	}
	return nil
}
