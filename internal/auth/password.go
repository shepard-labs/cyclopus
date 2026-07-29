package auth

import (
	"crypto/rand"
	"crypto/subtle"
	"encoding/base64"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"sync"

	"golang.org/x/crypto/argon2"
)

const (
	maxPasswordBytes = 1024
	minMemoryKiB     = 8 * 1024
	maxMemoryKiB     = 128 * 1024
	minIterations    = 1
	maxIterations    = 5
	minParallelism   = 1
	maxParallelism   = 8
	minSaltBytes     = 16
	maxSaltBytes     = 32
	passwordKeyBytes = 32
)

var ErrInvalidCredential = errors.New("auth: invalid credential")

var passwordHashSlots = make(chan struct{}, 4)
var passwordHashSlotsMu sync.Mutex

func SetMaxConcurrentHashes(limit int) error {
	if limit <= 0 || limit > 64 {
		return errors.New("password hash concurrency must be between 1 and 64")
	}
	passwordHashSlotsMu.Lock()
	defer passwordHashSlotsMu.Unlock()
	passwordHashSlots = make(chan struct{}, limit)
	return nil
}

func withPasswordHashSlot(work func() []byte) []byte {
	passwordHashSlotsMu.Lock()
	slots := passwordHashSlots
	passwordHashSlotsMu.Unlock()
	slots <- struct{}{}
	defer func() { <-slots }()
	return work()
}

type PasswordParams struct {
	MemoryKiB   uint32
	Iterations  uint32
	Parallelism uint8
	SaltBytes   uint32
}

func DefaultPasswordParams() PasswordParams {
	return PasswordParams{MemoryKiB: 64 * 1024, Iterations: 3, Parallelism: 4, SaltBytes: 16}
}

func (p PasswordParams) validate() error {
	if p.MemoryKiB < minMemoryKiB || p.MemoryKiB > maxMemoryKiB {
		return fmt.Errorf("memory must be between %d and %d KiB", minMemoryKiB, maxMemoryKiB)
	}
	if p.Iterations < minIterations || p.Iterations > maxIterations {
		return fmt.Errorf("iterations must be between %d and %d", minIterations, maxIterations)
	}
	if p.Parallelism < minParallelism || p.Parallelism > maxParallelism {
		return fmt.Errorf("parallelism must be between %d and %d", minParallelism, maxParallelism)
	}
	if p.SaltBytes < minSaltBytes || p.SaltBytes > maxSaltBytes {
		return fmt.Errorf("salt must be between %d and %d bytes", minSaltBytes, maxSaltBytes)
	}
	return nil
}

func HashPassword(password string, params PasswordParams) (string, error) {
	if len(password) == 0 || len(password) > maxPasswordBytes {
		return "", ErrInvalidCredential
	}
	if err := params.validate(); err != nil {
		return "", fmt.Errorf("invalid Argon2id parameters: %w", err)
	}
	salt := make([]byte, params.SaltBytes)
	if _, err := rand.Read(salt); err != nil {
		return "", fmt.Errorf("generate password salt: %w", err)
	}
	key := withPasswordHashSlot(func() []byte {
		return argon2.IDKey([]byte(password), salt, params.Iterations,
			params.MemoryKiB, params.Parallelism, passwordKeyBytes)
	})
	return fmt.Sprintf("$argon2id$v=%d$m=%d,t=%d,p=%d$%s$%s", argon2.Version,
		params.MemoryKiB, params.Iterations, params.Parallelism,
		base64.RawStdEncoding.EncodeToString(salt), base64.RawStdEncoding.EncodeToString(key)), nil
}

func VerifyPassword(password, encoded string) (bool, error) {
	if len(password) == 0 || len(password) > maxPasswordBytes {
		return false, ErrInvalidCredential
	}
	params, salt, expected, err := parsePasswordHash(encoded)
	if err != nil {
		return false, err
	}
	actual := withPasswordHashSlot(func() []byte {
		return argon2.IDKey([]byte(password), salt, params.Iterations,
			params.MemoryKiB, params.Parallelism, uint32(len(expected)))
	})
	return subtle.ConstantTimeCompare(actual, expected) == 1, nil
}

func parsePasswordHash(encoded string) (PasswordParams, []byte, []byte, error) {
	parts := strings.Split(encoded, "$")
	if len(parts) != 6 || parts[0] != "" || parts[1] != "argon2id" || parts[2] != "v=19" {
		return PasswordParams{}, nil, nil, ErrInvalidCredential
	}
	values := strings.Split(parts[3], ",")
	if len(values) != 3 {
		return PasswordParams{}, nil, nil, ErrInvalidCredential
	}
	memory, err := parseUintParameter(values[0], "m=", 32)
	if err != nil {
		return PasswordParams{}, nil, nil, ErrInvalidCredential
	}
	iterations, err := parseUintParameter(values[1], "t=", 32)
	if err != nil {
		return PasswordParams{}, nil, nil, ErrInvalidCredential
	}
	parallelism, err := parseUintParameter(values[2], "p=", 8)
	if err != nil {
		return PasswordParams{}, nil, nil, ErrInvalidCredential
	}
	params := PasswordParams{MemoryKiB: uint32(memory), Iterations: uint32(iterations),
		Parallelism: uint8(parallelism)}
	salt, err := base64.RawStdEncoding.Strict().DecodeString(parts[4])
	if err != nil || len(salt) < minSaltBytes || len(salt) > maxSaltBytes {
		return PasswordParams{}, nil, nil, ErrInvalidCredential
	}
	params.SaltBytes = uint32(len(salt))
	if err := params.validate(); err != nil {
		return PasswordParams{}, nil, nil, ErrInvalidCredential
	}
	expected, err := base64.RawStdEncoding.Strict().DecodeString(parts[5])
	if err != nil || len(expected) != passwordKeyBytes {
		return PasswordParams{}, nil, nil, ErrInvalidCredential
	}
	return params, salt, expected, nil
}

func parseUintParameter(value, prefix string, bits int) (uint64, error) {
	if !strings.HasPrefix(value, prefix) || len(value) == len(prefix) {
		return 0, ErrInvalidCredential
	}
	return strconv.ParseUint(value[len(prefix):], 10, bits)
}
