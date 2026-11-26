package settingstest

// The configuration space of FIN (F1 to F9) and SEA (S1) that the combination tests cover: each item with its values, F1's
// threshold at two levels (ThresholdLow below the amounts of box A, ThresholdHigh above them; box A's former share is CNY
// 500.00, 50,000 minor units).
const (
	ThresholdLevelLow  = "NOT_ABOVE_THRESHOLD@LOW"
	ThresholdLevelHigh = "NOT_ABOVE_THRESHOLD@HIGH"
	ThresholdLow       = int64(10_000)
	ThresholdHigh      = int64(100_000)
)

// Items are the factors of the space, in the order of the catalogues; a test checks that they are the catalogues' items and
// values (internal/bootstrap).
var Items = []Factor{
	{"fin.F1", []string{"NOT_ALLOWED", "UNRESTRICTED", ThresholdLevelLow, ThresholdLevelHigh}},
	{"fin.F2", []string{"SUBMITTER_AND_PREVIEWER", "SUBMITTER_ONLY"}},
	{"fin.F3", []string{"EVERY_ATTRIBUTION", "ONLY_WHEN_AMOUNT_MOVES"}},
	{"fin.F4", []string{"ALL_PREVIEWED", "ONE_STEP_WITHOUT_OPENED_OR_LEFT"}},
	{"fin.F6", []string{"SEPARATE", "INCLUDED_MARKED"}},
	{"fin.F7", []string{"BLOCKS", "DOES_NOT_BLOCK"}},
	{"fin.F8", []string{"REJECT_WITH_DEFAULT", "DEFAULT_TO_REMAINING"}},
	{"fin.F9", []string{"RECONFIRM", "KEEP_RATIO", "KEEP_AMOUNT"}},
	{"sea.S1", []string{"SUPERVISOR_ONLY", "WITH_REASON"}},
}

// ValidConfiguration is the rule across items: F8 DEFAULT_TO_REMAINING goes only with F3 ONLY_WHEN_AMOUNT_MOVES.
func ValidConfiguration(m map[string]string) bool {
	return !(m["fin.F8"] == "DEFAULT_TO_REMAINING" && m["fin.F3"] != "ONLY_WHEN_AMOUNT_MOVES")
}

// Configurations is the pairwise covering array of the space.
func Configurations() []Row { return Pairwise(Items, ValidConfiguration) }

// Config is one configuration by item key.
type Config map[string]string

// ConfigOf answers a row as a configuration.
func ConfigOf(r Row) Config { return Config(asMap(Items, r)) }

// ThresholdOf answers the F1 threshold of a configuration in minor units, 0 when F1 has none.
func (c Config) ThresholdOf() int64 {
	switch c["fin.F1"] {
	case ThresholdLevelLow:
		return ThresholdLow
	case ThresholdLevelHigh:
		return ThresholdHigh
	}
	return 0
}

// F1Value is F1's value without the level of its threshold.
func (c Config) F1Value() string {
	if c.ThresholdOf() > 0 {
		return "NOT_ABOVE_THRESHOLD"
	}
	return c["fin.F1"]
}

// ArrayText writes the covering array as the domain tests read it (they cannot import this package): a line "factor
// <item>: <level>,<level>..." for each item, then a line of "<item>=<level>" pairs for each configuration.
func ArrayText() string {
	var b []byte
	for _, f := range Items {
		b = append(b, "factor "+f.Name+": "...)
		for i, l := range f.Levels {
			if i > 0 {
				b = append(b, ',')
			}
			b = append(b, l...)
		}
		b = append(b, '\n')
	}
	for _, r := range Configurations() {
		for i, f := range Items {
			if i > 0 {
				b = append(b, ' ')
			}
			b = append(b, f.Name+"="+f.Levels[r[i]]...)
		}
		b = append(b, '\n')
	}
	return string(b)
}
