package commerce

import (
	"errors"
	"math/big"
	"strings"
)

const gigabyte = int64(1 << 30)

// Convert human-facing plan fields to the integer units used internally.
func applyPlanUnits(p *Plan) error {
	if p.PriceYuan != nil {
		value, err := scaledInteger(*p.PriceYuan, 100)
		if err != nil {
			return errors.New("invalid plan price")
		}
		p.Price = value
		p.PriceYuan = nil
	}
	if p.QuotaGB != nil {
		raw := strings.TrimSpace(*p.QuotaGB)
		if raw == "" {
			p.Quota = 0
		} else {
			value, err := scaledInteger(raw, gigabyte)
			if err != nil {
				return errors.New("invalid plan quota")
			}
			p.Quota = value
		}
		p.QuotaGB = nil
	}
	return nil
}

func scaledInteger(raw string, scale int64) (int64, error) {
	value, ok := new(big.Rat).SetString(strings.TrimSpace(raw))
	if !ok || value.Sign() < 0 {
		return 0, errors.New("invalid decimal")
	}
	value.Mul(value, new(big.Rat).SetInt64(scale))
	if !value.IsInt() || !value.Num().IsInt64() {
		return 0, errors.New("decimal precision exceeds storage unit")
	}
	return value.Num().Int64(), nil
}
