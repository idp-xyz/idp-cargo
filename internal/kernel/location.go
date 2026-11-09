package kernel

// LocationScheme is the code list a LocationCode comes from.
type LocationScheme string

const (
	// UNLOCODE is UN/ECE Recommendation 16: a two-letter country code and three letters or digits 2 to 9,
	// such as CNSHA for Shanghai.
	UNLOCODE LocationScheme = "UNLOCODE"
	// IATAAirport is a three-letter IATA airport code, such as PVG for Shanghai Pudong.
	IATAAirport LocationScheme = "IATA"
)

// LocationCode is a location in one code list. The scheme is part of the value, so a UN/LOCODE and an
// IATA airport code are never confused. The kernel checks only the form; whether a location exists
// belongs to reference data (MD-26).
type LocationCode struct {
	Scheme LocationScheme
	Code   string
}

// Valid reports whether the code has the form of its scheme.
func (l LocationCode) Valid() bool {
	switch l.Scheme {
	case UNLOCODE:
		if len(l.Code) != 5 || !upper(l.Code[0]) || !upper(l.Code[1]) {
			return false
		}
		for i := 2; i < 5; i++ {
			if !upper(l.Code[i]) && (l.Code[i] < '2' || l.Code[i] > '9') {
				return false
			}
		}
		return true
	case IATAAirport:
		return len(l.Code) == 3 && upper(l.Code[0]) && upper(l.Code[1]) && upper(l.Code[2])
	}
	return false
}

// IsZero reports whether no location is given.
func (l LocationCode) IsZero() bool { return l == LocationCode{} }

func upper(b byte) bool { return b >= 'A' && b <= 'Z' }
