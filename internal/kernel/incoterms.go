package kernel

// incoterms lists the rules of each Incoterms edition the ICC publishes that cargo accepts (decision C8):
// Incoterms 2020 replaced DAT by DPU.
var incoterms = map[string]map[string]bool{
	"2020": {"EXW": true, "FCA": true, "CPT": true, "CIP": true, "DAP": true, "DPU": true, "DDP": true, "FAS": true, "FOB": true, "CFR": true, "CIF": true},
	"2010": {"EXW": true, "FCA": true, "CPT": true, "CIP": true, "DAT": true, "DAP": true, "DDP": true, "FAS": true, "FOB": true, "CFR": true, "CIF": true},
}

// ValidIncoterm reports whether rule, such as FOB, is a rule of the Incoterms edition, 2010 or 2020.
func ValidIncoterm(edition, rule string) bool { return incoterms[edition][rule] }
