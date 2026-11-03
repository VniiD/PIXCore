// Package events define o envelope CloudEvents e a assinatura HMAC das mensagens do PixCore.
package events

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"
)

const SignatureHeader = "pixcore-signature"

// MaxEnvelopeBytes limita mensagens aceitas (defesa contra payloads abusivos).
const MaxEnvelopeBytes = 64 * 1024

type Envelope struct {
	SpecVersion     string          `json:"specversion"`
	ID              string          `json:"id"`
	Type            string          `json:"type"`
	Source          string          `json:"source"`
	Subject         string          `json:"subject,omitempty"`
	Time            time.Time       `json:"time"`
	DataContentType string          `json:"datacontenttype"`
	Data            json.RawMessage `json:"data"`
}

func New(id, typ, source, subject string, data any) (Envelope, error) {
	raw, err := json.Marshal(data)
	if err != nil {
		return Envelope{}, err
	}
	return Envelope{
		SpecVersion: "1.0", ID: id, Type: typ, Source: source, Subject: subject,
		Time: time.Now().UTC(), DataContentType: "application/json", Data: raw,
	}, nil
}

func Decode(b []byte) (Envelope, error) {
	if len(b) > MaxEnvelopeBytes {
		return Envelope{}, errors.New("envelope too large")
	}
	var e Envelope
	if err := json.Unmarshal(b, &e); err != nil {
		return Envelope{}, fmt.Errorf("decode envelope: %w", err)
	}
	if e.SpecVersion != "1.0" || e.ID == "" || e.Type == "" || len(e.Data) == 0 {
		return Envelope{}, errors.New("malformed envelope")
	}
	return e, nil
}

// Keyring guarda chaves HMAC por id (permite rotação: várias válidas para verificar, uma para assinar).
type Keyring struct {
	signID string
	keys   map[string][]byte
}

func NewKeyring(signID string, keys map[string][]byte) (*Keyring, error) {
	for id, k := range keys {
		if len(k) < 32 {
			return nil, fmt.Errorf("hmac key %q must have at least 32 bytes", id)
		}
	}
	if signID != "" {
		if _, ok := keys[signID]; !ok {
			return nil, fmt.Errorf("signing key %q not in keyring", signID)
		}
	}
	return &Keyring{signID: signID, keys: keys}, nil
}

func mac(key, payload []byte) string {
	m := hmac.New(sha256.New, key)
	m.Write(payload)
	return hex.EncodeToString(m.Sum(nil))
}

// Sign devolve o valor do header de assinatura, ou "" se não há chave de assinatura.
func (k *Keyring) Sign(payload []byte) string {
	if k == nil || k.signID == "" {
		return ""
	}
	return "kid=" + k.signID + ",sig=" + mac(k.keys[k.signID], payload)
}

var ErrBadSignature = errors.New("invalid message signature")

// Verify valida o header em tempo constante. Keyring nulo ou sem chaves não verifica nada.
func (k *Keyring) Verify(payload []byte, header string) error {
	if k == nil || len(k.keys) == 0 {
		return nil
	}
	var kid, sig string
	for _, part := range strings.Split(header, ",") {
		switch {
		case strings.HasPrefix(part, "kid="):
			kid = part[4:]
		case strings.HasPrefix(part, "sig="):
			sig = part[4:]
		}
	}
	key, ok := k.keys[kid]
	if !ok || sig == "" {
		return ErrBadSignature
	}
	want := mac(key, payload)
	if !hmac.Equal([]byte(want), []byte(sig)) {
		return ErrBadSignature
	}
	return nil
}

// Enabled informa se a verificação está ativa.
func (k *Keyring) Enabled() bool { return k != nil && len(k.keys) > 0 }
