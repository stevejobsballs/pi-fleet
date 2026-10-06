package domain

import (
	"errors"
	"fmt"
	"math/big"
	"regexp"
)

// Measurements travel as decimal strings and are compared with exact
// rational arithmetic, so pass/fail never depends on floating-point
// rounding and central's recomputation matches bit for bit.

var decimalRE = regexp.MustCompile(`^-?(0|[1-9][0-9]*)(\.[0-9]+)?$`)

func parseDecimal(s string) (*big.Rat, error) {
	if !decimalRE.MatchString(s) {
		return nil, fmt.Errorf("%q is not a plain decimal number", s)
	}
	r, ok := new(big.Rat).SetString(s)
	if !ok {
		return nil, fmt.Errorf("%q is not a number", s)
	}
	return r, nil
}

// Tolerance kinds.
const (
	TolAbsolute = "abs"    // nominal ± value
	TolPercent  = "pct"    // nominal ± value% of |nominal|
	TolLimits   = "limits" // lower ≤ x ≤ upper
)

// Tolerance is a calibration point's acceptance criterion.
type Tolerance struct {
	Kind  string `json:"kind"`
	Value string `json:"value,omitempty"`
	Lower string `json:"lower,omitempty"`
	Upper string `json:"upper,omitempty"`
}

// Limits returns the inclusive acceptance interval for a nominal value.
func (t Tolerance) Limits(nominal string) (lo, hi *big.Rat, err error) {
	switch t.Kind {
	case TolAbsolute, TolPercent:
		if t.Lower != "" || t.Upper != "" {
			return nil, nil, fmt.Errorf("%s tolerance takes only a value", t.Kind)
		}
		n, err := parseDecimal(nominal)
		if err != nil {
			return nil, nil, fmt.Errorf("nominal: %w", err)
		}
		v, err := parseDecimal(t.Value)
		if err != nil {
			return nil, nil, fmt.Errorf("tolerance: %w", err)
		}
		if v.Sign() < 0 {
			return nil, nil, errors.New("tolerance must not be negative")
		}
		dev := v
		if t.Kind == TolPercent {
			abs := new(big.Rat).Abs(n)
			dev = new(big.Rat).Mul(abs, new(big.Rat).Quo(v, big.NewRat(100, 1)))
		}
		return new(big.Rat).Sub(n, dev), new(big.Rat).Add(n, dev), nil
	case TolLimits:
		if t.Value != "" {
			return nil, nil, errors.New("limits tolerance takes lower and upper, not a value")
		}
		lo, err := parseDecimal(t.Lower)
		if err != nil {
			return nil, nil, fmt.Errorf("lower limit: %w", err)
		}
		hi, err := parseDecimal(t.Upper)
		if err != nil {
			return nil, nil, fmt.Errorf("upper limit: %w", err)
		}
		if lo.Cmp(hi) > 0 {
			return nil, nil, errors.New("lower limit exceeds upper limit")
		}
		return lo, hi, nil
	}
	return nil, nil, fmt.Errorf("unknown tolerance kind %q", t.Kind)
}

// within reports whether reading lies in [lo, hi].
func within(reading string, lo, hi *big.Rat) (bool, error) {
	r, err := parseDecimal(reading)
	if err != nil {
		return false, err
	}
	return r.Cmp(lo) >= 0 && r.Cmp(hi) <= 0, nil
}

// formatLimit renders a computed limit exactly when it terminates, or to
// 12 decimal places otherwise (only possible for percentage tolerances).
func formatLimit(r *big.Rat) string {
	if s, exact := r.FloatPrec(); exact {
		return r.FloatString(s)
	}
	return r.FloatString(12)
}
