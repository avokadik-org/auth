package crypto

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"math"
	"math/big"
	"net/url"
	"strconv"
	"strings"

	"crypto/ed25519"
	"time"

	"golang.org/x/crypto/hkdf"

	"github.com/btcsuite/btcutil/base58"
	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/crypto"
	siws "github.com/supabase/auth/internal/utilities/solana"
)

// SecureToken creates a new random token
func SecureToken() string {
	b := make([]byte, 16)
	must(io.ReadFull(rand.Reader, b))

	return base64.RawURLEncoding.EncodeToString(b)
}

// GenerateOtp generates a random n digit otp
func GenerateOtp(digits int) string {
	upper := math.Pow10(digits)
	val := must(rand.Int(rand.Reader, big.NewInt(int64(upper))))

	// adds a variable zero-padding to the left to ensure otp is uniformly random
	expr := "%0" + strconv.Itoa(digits) + "v"
	otp := fmt.Sprintf(expr, val.String())

	return otp
}
func GenerateTokenHash(emailOrPhone, otp string) string {
	return fmt.Sprintf("%x", sha256.Sum224([]byte(emailOrPhone+otp)))
}

// Generated a random secure integer from [0, max[
func secureRandomInt(max int) int {
	randomInt := must(rand.Int(rand.Reader, big.NewInt(int64(max))))
	return int(randomInt.Int64())
}

type EncryptedString struct {
	KeyID     string `json:"key_id"`
	Algorithm string `json:"alg"`
	Data      []byte `json:"data"`
	Nonce     []byte `json:"nonce,omitempty"`
}

func (es *EncryptedString) IsValid() bool {
	return es.KeyID != "" && len(es.Data) > 0 && len(es.Nonce) > 0 && es.Algorithm == "aes-gcm-hkdf"
}

// ShouldReEncrypt tells you if the value encrypted needs to be encrypted again with a newer key.
func (es *EncryptedString) ShouldReEncrypt(encryptionKeyID string) bool {
	return es.KeyID != encryptionKeyID
}

func (es *EncryptedString) Decrypt(id string, decryptionKeys map[string]string) ([]byte, error) {
	decryptionKey := decryptionKeys[es.KeyID]

	if decryptionKey == "" {
		return nil, fmt.Errorf("crypto: decryption key with name %q does not exist", es.KeyID)
	}

	key, err := deriveSymmetricKey(id, es.KeyID, decryptionKey)
	if err != nil {
		return nil, err
	}

	block := must(aes.NewCipher(key))
	cipher := must(cipher.NewGCM(block))

	decrypted, err := cipher.Open(nil, es.Nonce, es.Data, nil) // #nosec G407
	if err != nil {
		return nil, err
	}

	return decrypted, nil
}

func ParseEncryptedString(str string) *EncryptedString {
	if !strings.HasPrefix(str, "{") {
		return nil
	}

	var es EncryptedString

	if err := json.Unmarshal([]byte(str), &es); err != nil {
		return nil
	}

	if !es.IsValid() {
		return nil
	}

	return &es
}

func (es *EncryptedString) String() string {
	out := must(json.Marshal(es))

	return string(out)
}

func deriveSymmetricKey(id, keyID, keyBase64URL string) ([]byte, error) {
	hkdfKey, err := base64.RawURLEncoding.DecodeString(keyBase64URL)
	if err != nil {
		return nil, err
	}

	if len(hkdfKey) != 256/8 {
		return nil, fmt.Errorf("crypto: key with ID %q is not 256 bits", keyID)
	}

	// Since we use AES-GCM here, the same symmetric key *must not be used
	// more than* 2^32 times. But, that's not that much. Suppose a system
	// with 100 million users, then a user can only change their password
	// 42 times. To prevent this, the actual symmetric key is derived by
	// using HKDF using the encryption key and the "ID" of the object
	// containing the encryption string. Ideally this ID is a UUID.  This
	// has the added benefit that the encrypted string is bound to that
	// specific object, and can't accidentally be "moved" to other objects
	// without changing their ID to the original one.

	keyReader := hkdf.New(sha256.New, hkdfKey, nil, []byte(id))
	key := make([]byte, 256/8)

	must(io.ReadFull(keyReader, key))

	return key, nil
}

func NewEncryptedString(id string, data []byte, keyID string, keyBase64URL string) (*EncryptedString, error) {
	key, err := deriveSymmetricKey(id, keyID, keyBase64URL)
	if err != nil {
		return nil, err
	}

	block := must(aes.NewCipher(key))
	cipher := must(cipher.NewGCM(block))

	es := EncryptedString{
		KeyID:     keyID,
		Algorithm: "aes-gcm-hkdf",
		Nonce:     make([]byte, 12),
	}

	must(io.ReadFull(rand.Reader, es.Nonce))
	es.Data = cipher.Seal(nil, es.Nonce, data, nil) // #nosec G407

	return &es, nil
}

func VerifySIWS(
    rawMessage string,
    signature []byte,
    msg *siws.SIWSMessage,
    params siws.SIWSVerificationParams,
) error {
    // 1) Basic input validation
    if rawMessage == "" {
        return siws.ErrEmptyRawMessage
    }
    if len(signature) == 0 {
        return siws.ErrEmptySignature
    }
    if msg == nil {
        return siws.ErrNilMessage
    }

    // 2) Domain validation
    if params.ExpectedDomain == "" {
        return siws.ErrMissingDomain
    }
    if !siws.IsValidDomain(msg.Domain) {
        return siws.ErrInvalidDomainFormat
    }
    if msg.Domain != params.ExpectedDomain {
        return siws.ErrDomainMismatch
    }

    // 3) Address/Public Key validation (combined checks)
    pubKey := base58.Decode(msg.Address)
    if !siws.IsBase58PubKey(pubKey) {
        return siws.ErrInvalidPubKeySize
    }

    // 4) Version validation
    if msg.Version != "1" {
        return siws.ErrInvalidVersion
    }

    // 5) Chain ID validation (using helper)
    if msg.ChainID != "" {
        if !siws.IsValidSolanaNetwork(msg.ChainID) { 
			
            return siws.ErrInvalidChainID
        }
    }

    // 6) Nonce validation (consolidated)
    if msg.Nonce != "" {
        if len(msg.Nonce) < 8 {
            return siws.ErrNonceTooShort
        }
    }

    // 7) URI and Resources validation
    if msg.URI != "" {
        if _, err := url.Parse(msg.URI); err != nil {
            return siws.ErrInvalidURI
        }
    }

    for _, resource := range msg.Resources {
        if _, err := url.Parse(resource); err != nil {
            return siws.ErrInvalidResourceURI
        }
    }

    // 8) Signature verification
    if !ed25519.Verify(pubKey, []byte(rawMessage), signature) {
        return siws.ErrSignatureVerification
    }

    // 9) Time validations (consolidated)
    now := time.Now().UTC()

    if !msg.IssuedAt.IsZero() {
        if now.Before(msg.IssuedAt) {
            return siws.ErrFutureMessage
        }

        if params.CheckTime && params.TimeDuration > 0 {
            expiry := msg.IssuedAt.Add(params.TimeDuration)
            if now.After(expiry) {
                return siws.ErrMessageExpired
            }
        }
    }

    if !msg.NotBefore.IsZero() && now.Before(msg.NotBefore) {
        return siws.ErrNotYetValid
    }

    if !msg.ExpirationTime.IsZero() && now.After(msg.ExpirationTime) {
        return siws.ErrMessageExpired
    }

    return nil
}

func VerifyEthereumSignature(message string, signature string, address string) error {
	// Remove 0x prefix if present
	signature = removeHexPrefix(signature)
	address = removeHexPrefix(address)

	// Convert signature hex to bytes
	sigBytes, err := hex.DecodeString(signature)
	if err != nil {
		return fmt.Errorf("siwe: invalid signature hex: %w", err)
	}

	// Adjust V value in signature (Ethereum specific)
	if len(sigBytes) != 65 {
		return fmt.Errorf("siwe: invalid signature length")
	}
	if sigBytes[64] < 27 {
		sigBytes[64] += 27
	}

	// Hash the message according to EIP-191
	prefixedMessage := fmt.Sprintf("\x19Ethereum Signed Message:\n%d%s", len(message), message)
	hash := crypto.Keccak256Hash([]byte(prefixedMessage))

	// Recover public key from signature
	pubKey, err := crypto.SigToPub(hash.Bytes(), sigBytes)
	if err != nil {
		return fmt.Errorf("siwe: error recovering public key: %w", err)
	}

	// Derive Ethereum address from public key
	recoveredAddr := crypto.PubkeyToAddress(*pubKey)
	checkAddr := common.HexToAddress(address)

	// Compare addresses
	if recoveredAddr != checkAddr {
		return fmt.Errorf("siwe: signature not from expected address")
	}

	return nil
}

func removeHexPrefix(signature string) string {
	if strings.HasPrefix(signature, "0x") {
		return strings.TrimPrefix(signature, "0x")
	}
	return signature
}


