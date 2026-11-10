// Package api is the contract of Cargo's read-only reference data (repository structure baseline §11, MD-26):
// locations by UN/LOCODE with their functions, IATA airport codes and the aliases carriers use, by edition.
// Contexts use reference data only through this package. It imports only the standard library.
package api

import (
	"context"
	"errors"
)

// Transport modes a location is resolved for.
const (
	ModeAir  = "AIR"
	ModeSea  = "SEA"
	ModeRoad = "ROAD"
	ModeRail = "RAIL"
)

// Schemes a location may be given in.
const (
	SchemeUNLOCODE = "UNLOCODE"
	SchemeIATA     = "IATA"
)

// How a location was resolved.
const (
	ViaUNLOCODE = "UNLOCODE" // the UN/LOCODE itself
	ViaIATA     = "IATA"     // an IATA airport code
	ViaAlias    = "ALIAS"    // an alias carriers use for the mode, such as CNSHA for the port of Shanghai
)

// ResolveLocation asks for the location a code names for a transport mode (decision F2, option 4). Code is a
// UN/LOCODE, or for AIR also an IATA airport code (Scheme IATA).
type ResolveLocation struct {
	Scheme string
	Code   string
	Mode   string
}

// Location is a resolved location of the current edition. Functions are the UN/LOCODE function classifiers
// (eight positions: 1 port, 2 rail, 3 road, 4 airport, 5 postal, 6 multimodal, 7 fixed transport, 8 border
// crossing). IATAAirport is set when the reference data maps the location to exactly one IATA airport. Every
// AIR location has one; city-level codes of several airports (USNYC, JPTYO) have none and are refused for AIR.
type Location struct {
	UNLocode    string
	Name        string
	CountryCode string
	Functions   string
	IATAAirport string
	Edition     string
	Via         string
}

// Locations resolves codes against the current edition of the location reference data.
type Locations interface {
	ResolveLocation(ctx context.Context, q ResolveLocation) (Location, error)
}

// Error kinds. A code the reference data does not know, or that names several locations, is
// ErrUnknownLocation or ErrAmbiguousLocation; a location without the function the mode needs (an airport for
// AIR, a port for SEA) is ErrNotForMode.
var (
	ErrUnknownLocation   = errors.New("reference: no such location in the current edition")
	ErrAmbiguousLocation = errors.New("reference: the code names more than one location")
	ErrNotForMode        = errors.New("reference: the location does not serve the transport mode")
	ErrInvalidQuery      = errors.New("reference: invalid location query")
)
