package auth

import (
	"fmt"

	"golang.org/x/crypto/bcrypt"

	"awesomeProject/internal/domain"
)

const (
	MinPasswordLength = 8
	MaxPasswordBytes  = 72
)

func validatePassword(pw string) error {
	if len(pw) < MinPasswordLength || len(pw) > MaxPasswordBytes {
		return fmt.Errorf("%w: password must be %d..%d bytes", domain.ErrValidation, MinPasswordLength, MaxPasswordBytes)
	}
	return nil
}

type hasher struct {
	cost  int
	dummy []byte
}

func newHasher(cost int) (*hasher, error) {
	if cost == 0 {
		cost = bcrypt.DefaultCost
	}
	if cost < bcrypt.MinCost || cost > bcrypt.MaxCost {
		return nil, fmt.Errorf("bcrypt cost must be in %d..%d", bcrypt.MinCost, bcrypt.MaxCost)
	}
	dummy, err := bcrypt.GenerateFromPassword([]byte("timing-equalizer"), cost)
	if err != nil {
		return nil, err
	}
	return &hasher{cost: cost, dummy: dummy}, nil
}

func (h *hasher) hash(pw string) (string, error) {
	b, err := bcrypt.GenerateFromPassword([]byte(pw), h.cost)
	return string(b), err
}

func (h *hasher) verify(hash, pw string) bool {
	return bcrypt.CompareHashAndPassword([]byte(hash), []byte(pw)) == nil
}

func (h *hasher) burn(pw string) {
	_ = bcrypt.CompareHashAndPassword(h.dummy, []byte(pw))
}
