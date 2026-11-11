package kernel

import "math/big"

// ConvertMinor converts an amount in minor units of one currency into minor units of another at a rate given
// as an integer with a scale: one unit of the first currency is rate × 10^−scale units of the second, and
// fromDigits and toDigits are the two currencies' ISO 4217 minor digits. It returns amount × rate ×
// 10^(toDigits − fromDigits − scale), halves rounded away from zero, so each converted line can be recomputed
// from the rate alone (RATE's quote rates, FIN's rate entries).
func ConvertMinor(amount, rate int64, scale, fromDigits, toDigits int) (int64, error) {
	num := new(big.Int).Mul(big.NewInt(amount), big.NewInt(rate))
	num.Mul(num, pow10(toDigits))
	den := new(big.Int).Mul(pow10(fromDigits), pow10(scale))
	q, rem := new(big.Int).QuoRem(num, den, new(big.Int))
	if twice := new(big.Int).Mul(new(big.Int).Abs(rem), big.NewInt(2)); twice.Cmp(den) >= 0 {
		q.Add(q, big.NewInt(int64(num.Sign())))
	}
	if !q.IsInt64() {
		return 0, ErrAmountOverflow
	}
	return q.Int64(), nil
}

func pow10(n int) *big.Int { return new(big.Int).Exp(big.NewInt(10), big.NewInt(int64(n)), nil) }
