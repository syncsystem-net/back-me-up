// Package keyring turns a passphrase held in .env into an AES-256-GCM key and
// seals credential material with it.
//
// The threat it addresses is specific: the metadata SQLite database is uploaded
// to every configured main account after every successful job, so that file
// routinely leaves this machine. Cloud passwords and OAuth tokens living in it
// in the clear would travel with it. Sealing them means the copy in the cloud is
// useless without the passphrase.
//
// That is also the whole cost of the design: the passphrase is never written to
// the database it protects, so losing it loses every stored credential. There is
// no recovery path, by construction — anything that could recover the key would
// have to travel with the backup too.
//
// Key derivation is scrypt over the passphrase plus a random per-install salt
// stored in the database. The parameters are stored alongside the salt rather
// than hard-coded at the open site, so raising them later does not make existing
// installs underivable.
package keyring

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"

	"golang.org/x/crypto/scrypt"

	"github.com/syncsystem-net/back-me-up/internal/database"
)

// EnvKey is the .env entry holding the passphrase. Named here so every message
// that mentions it — startup log, locked-mode banner, README — spells it the
// same way.
const EnvKey = "BACKMEUP_CREDENTIAL_PASSPHRASE"

var (
	// ErrNoPassphrase means EnvKey is unset or blank. Distinguished from a wrong
	// passphrase because the remedy differs and the UI says so.
	ErrNoPassphrase = errors.New("no credential passphrase configured")

	// ErrWrongPassphrase means a passphrase was supplied but does not match the
	// one this database was sealed with. Detected up front against a stored
	// verifier rather than discovered later as an unopenable credential.
	ErrWrongPassphrase = errors.New("credential passphrase does not match this database")

	// ErrCorrupt means a stored value is not a well-formed sealed blob, or its
	// authentication tag fails for a reason other than a wrong key (tampering,
	// truncation, a row copied over another row).
	ErrCorrupt = errors.New("stored credential is corrupt or was tampered with")
)

// Settings keys holding the crypto material. These live in the settings table
// for convenience, but the settings HTTP API serves a fixed whitelist of keys
// and must never expose or accept these: a PUT that could overwrite the salt
// would render every stored credential unopenable.
const (
	SettingSalt     = "credentials.kdf_salt"
	SettingParams   = "credentials.kdf_params"
	SettingVerifier = "credentials.verifier"
)

// verifierPlaintext is sealed at install time and opened at every start. Its
// content is irrelevant; being able to open it is the proof that the passphrase
// matches. It is not a secret.
const verifierPlaintext = "back-me-up credential keyring v1"

// verifierAAD binds the verifier blob to its purpose, so it cannot be moved into
// an account row (or vice versa) and still open.
const verifierAAD = "keyring/verifier"

// params are the scrypt cost parameters. N=32768/r=8/p=1 is the widely used
// interactive-login profile: ~32 MB and roughly a tenth of a second to derive,
// paid once at startup. Stored per install so these can be raised later without
// stranding databases sealed under the old values.
type params struct {
	KDF    string `json:"kdf"`
	N      int    `json:"n"`
	R      int    `json:"r"`
	P      int    `json:"p"`
	KeyLen int    `json:"key_len"`
}

func defaultParams() params {
	return params{KDF: "scrypt", N: 1 << 15, R: 8, P: 1, KeyLen: 32}
}

// Keyring seals and opens credential blobs. It holds the derived key and nothing
// that identifies the passphrase.
type Keyring struct {
	aead cipher.AEAD
}

// Open derives the key for this database from passphrase.
//
// On a database with no salt yet it performs the one-time install: a random
// 32-byte salt, the current parameters, and a sealed verifier. On a database
// that already has one it derives with the stored salt and parameters and proves
// the passphrase by opening the verifier, so a wrong passphrase is reported here
// rather than surfacing later as an unreadable account.
//
// A blank passphrase returns ErrNoPassphrase without touching the database:
// nothing may be initialised, re-sealed or overwritten unless the caller can
// prove it holds the right key.
func Open(db *sql.DB, passphrase string) (*Keyring, error) {
	if passphrase == "" {
		return nil, ErrNoPassphrase
	}

	saltHex, err := database.GetSetting(db, SettingSalt)
	if err != nil {
		return nil, fmt.Errorf("reading credential salt: %w", err)
	}
	if saltHex == "" {
		// Installing generates a new salt, which derives a different key. If sealed
		// credentials already exist, that would quietly make every one of them
		// unopenable — and the next .env import would re-seal over the ciphertext,
		// destroying an app-written token for good. A missing salt beside existing
		// credentials is a damaged database, so say so instead.
		sealed, err := database.HasSealedCredentials(db)
		if err != nil {
			return nil, err
		}
		if sealed {
			return nil, fmt.Errorf("%w: this database holds encrypted credentials but its key salt is missing, so they cannot be decrypted", ErrCorrupt)
		}
		return install(db, passphrase)
	}

	salt, err := hex.DecodeString(saltHex)
	if err != nil {
		return nil, fmt.Errorf("credential salt is not valid hex: %w", err)
	}
	p, err := loadParams(db)
	if err != nil {
		return nil, err
	}
	k, err := derive(passphrase, salt, p)
	if err != nil {
		return nil, err
	}

	verifierHex, err := database.GetSetting(db, SettingVerifier)
	if err != nil {
		return nil, fmt.Errorf("reading credential verifier: %w", err)
	}
	if verifierHex == "" {
		// A salt with no verifier can only come from an interrupted install. The
		// passphrase cannot be checked, so accept it and write the verifier now;
		// any credential sealed under a different key will fail to open on use.
		if err := writeVerifier(db, k); err != nil {
			return nil, err
		}
		return k, nil
	}
	blob, err := hex.DecodeString(verifierHex)
	if err != nil {
		return nil, fmt.Errorf("credential verifier is not valid hex: %w", err)
	}
	if _, err := k.OpenBlob(verifierAAD, blob); err != nil {
		return nil, ErrWrongPassphrase
	}
	return k, nil
}

// install performs the first-run setup: fresh salt, current parameters, sealed
// verifier.
//
// The salt is written LAST, because its presence is what Open treats as "this
// database has a keyring". An install interrupted halfway therefore looks
// uninstalled and is simply redone on the next start, rather than leaving a salt
// that no verifier corresponds to.
func install(db *sql.DB, passphrase string) (*Keyring, error) {
	salt := make([]byte, 32)
	if _, err := io.ReadFull(rand.Reader, salt); err != nil {
		return nil, fmt.Errorf("generating credential salt: %w", err)
	}
	p := defaultParams()
	k, err := derive(passphrase, salt, p)
	if err != nil {
		return nil, err
	}

	encoded, err := json.Marshal(p)
	if err != nil {
		return nil, fmt.Errorf("encoding kdf parameters: %w", err)
	}
	if err := database.SetSetting(db, SettingParams, string(encoded)); err != nil {
		return nil, err
	}
	if err := writeVerifier(db, k); err != nil {
		return nil, err
	}
	// Last, deliberately — see the doc comment.
	if err := database.SetSetting(db, SettingSalt, hex.EncodeToString(salt)); err != nil {
		return nil, err
	}
	return k, nil
}

func writeVerifier(db *sql.DB, k *Keyring) error {
	blob, err := k.SealBlob(verifierAAD, []byte(verifierPlaintext))
	if err != nil {
		return err
	}
	return database.SetSetting(db, SettingVerifier, hex.EncodeToString(blob))
}

func loadParams(db *sql.DB) (params, error) {
	raw, err := database.GetSetting(db, SettingParams)
	if err != nil {
		return params{}, fmt.Errorf("reading kdf parameters: %w", err)
	}
	if raw == "" {
		return defaultParams(), nil
	}
	var p params
	if err := json.Unmarshal([]byte(raw), &p); err != nil {
		return params{}, fmt.Errorf("decoding kdf parameters %q: %w", raw, err)
	}
	if p.KDF != "scrypt" {
		return params{}, fmt.Errorf("unsupported credential kdf %q (this build understands scrypt)", p.KDF)
	}
	if p.N <= 1 || p.R <= 0 || p.P <= 0 || p.KeyLen != 32 {
		return params{}, fmt.Errorf("invalid credential kdf parameters: %+v", p)
	}
	return p, nil
}

func derive(passphrase string, salt []byte, p params) (*Keyring, error) {
	key, err := scrypt.Key([]byte(passphrase), salt, p.N, p.R, p.P, p.KeyLen)
	if err != nil {
		return nil, fmt.Errorf("deriving credential key: %w", err)
	}
	block, err := aes.NewCipher(key)
	if err != nil {
		return nil, fmt.Errorf("creating cipher: %w", err)
	}
	aead, err := cipher.NewGCM(block)
	if err != nil {
		return nil, fmt.Errorf("creating gcm: %w", err)
	}
	return &Keyring{aead: aead}, nil
}

// SealBlob encrypts plaintext under a fresh random nonce and returns
// nonce||ciphertext.
//
// aad is authenticated but not encrypted, and callers pass the identity of the
// row the blob belongs to (see AccountAAD). A blob moved to a different row
// therefore fails to open instead of silently authenticating as the wrong
// account.
func (k *Keyring) SealBlob(aad string, plaintext []byte) ([]byte, error) {
	nonce := make([]byte, k.aead.NonceSize())
	if _, err := io.ReadFull(rand.Reader, nonce); err != nil {
		return nil, fmt.Errorf("generating nonce: %w", err)
	}
	// Appending to nonce would alias its backing array into the output; allocate
	// the result explicitly so the prefix is exactly the nonce we generated.
	out := make([]byte, 0, len(nonce)+len(plaintext)+k.aead.Overhead())
	out = append(out, nonce...)
	return k.aead.Seal(out, nonce, plaintext, []byte(aad)), nil
}

// OpenBlob reverses SealBlob. A blob that fails authentication yields ErrCorrupt
// wrapped with context; the caller cannot distinguish tampering from a wrong key
// here, which is why the passphrase is checked once against the verifier at
// startup instead.
func (k *Keyring) OpenBlob(aad string, blob []byte) ([]byte, error) {
	ns := k.aead.NonceSize()
	if len(blob) < ns+k.aead.Overhead() {
		return nil, fmt.Errorf("%w: sealed value is %d bytes, too short to be valid", ErrCorrupt, len(blob))
	}
	plaintext, err := k.aead.Open(nil, blob[:ns], blob[ns:], []byte(aad))
	if err != nil {
		return nil, fmt.Errorf("%w: %v", ErrCorrupt, err)
	}
	return plaintext, nil
}

// SealJSON marshals v and seals it. Credentials are stored as one sealed
// document per row rather than one sealed column per secret: a single nonce and
// a single authentication scope per account, and a provider's credential set can
// grow without a schema migration.
func (k *Keyring) SealJSON(aad string, v any) ([]byte, error) {
	b, err := json.Marshal(v)
	if err != nil {
		return nil, fmt.Errorf("encoding credentials: %w", err)
	}
	return k.SealBlob(aad, b)
}

// OpenJSON opens a blob written by SealJSON into v.
func (k *Keyring) OpenJSON(aad string, blob []byte, v any) error {
	b, err := k.OpenBlob(aad, blob)
	if err != nil {
		return err
	}
	if err := json.Unmarshal(b, v); err != nil {
		return fmt.Errorf("%w: decoding credentials: %v", ErrCorrupt, err)
	}
	return nil
}

// AccountAAD is the additional-authenticated-data string binding a sealed
// credential blob to the account it belongs to.
func AccountAAD(kind, provider, email string) string {
	return kind + "/" + provider + "/" + email
}
