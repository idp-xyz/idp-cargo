package domain

import (
	"errors"
	"fmt"
	"math/rand/v2"
	"slices"
	"strings"
	"testing"
	"time"
)

var bookAt = time.Date(2026, 11, 10, 9, 0, 0, 0, time.UTC)

func office(code string) Address {
	return Address{ShortCode: code, Purposes: []AddressPurpose{AddressOffice, AddressBilling}, Name: "Fixture Trading Co., Ltd.",
		Lines: []string{"Room 1001, 88 Century Avenue"}, City: "Shanghai", Postcode: "200120", CountryCode: "CN", UNLocode: "CNSHA",
		Local: &LocalAddress{Language: "zh", Name: "示例贸易有限公司", Lines: []string{"世纪大道88号1001室"}, City: "上海"}}
}

func warehouse(code string) Address {
	return Address{ShortCode: code, Purposes: []AddressPurpose{AddressPickup}, Name: "Fixture Warehouse",
		Lines: []string{"Gate 3", "1 Harbour Road"}, City: "Ningbo", CountryCode: "CN"}
}

func bookingDesk() Contact {
	return Contact{Purposes: []ContactPurpose{ContactBooking, ContactDocuments}, Name: "Test Person A", Email: "desk-a@example.test"}
}

func newBook(t *testing.T) *AddressBook {
	t.Helper()
	b, err := NewAddressBook("T", "P")
	if err != nil {
		t.Fatal(err)
	}
	return b
}

func TestAddressBookAddsAddressesWithDefaultsAndVersions(t *testing.T) {
	b := newBook(t)
	v, err := b.AddAddress("A1", office("HQ"), []AddressPurpose{AddressOffice}, "actor", bookAt)
	if err != nil || v.Version != 1 || v.Revision != 1 || b.Revision() != 1 {
		t.Fatalf("add: v=%+v rev=%d err=%v", v, b.Revision(), err)
	}
	if !slices.Equal(v.Purposes, []AddressPurpose{AddressBilling, AddressOffice}) {
		t.Fatalf("purposes are kept sorted: %v", v.Purposes)
	}
	if id, ok := b.DefaultAddress(AddressOffice); !ok || id != "A1" {
		t.Fatalf("default office = %q %v", id, ok)
	}
	if _, ok := b.DefaultAddress(AddressBilling); ok {
		t.Fatal("billing default was not asked for")
	}
	if _, err = b.AddAddress("A2", office("HQ"), nil, "actor", bookAt); !errors.Is(err, ErrShortCodeInUse) {
		t.Fatalf("second active HQ: %v", err)
	}
	if _, err = b.AddAddress("A1", warehouse("WH"), nil, "actor", bookAt); !errors.Is(err, ErrEntryExists) {
		t.Fatalf("reused id: %v", err)
	}
	if _, err = b.AddAddress("A2", warehouse("WH"), []AddressPurpose{AddressDelivery}, "actor", bookAt); !errors.Is(err, ErrInvalidAddress) {
		t.Fatalf("default for a purpose the address lacks: %v", err)
	}
	if _, err = b.AddAddress("A2", warehouse("WH"), []AddressPurpose{AddressPickup}, "actor", bookAt); err != nil {
		t.Fatal(err)
	}
	changed := office("HQ")
	changed.Purposes = []AddressPurpose{AddressBilling}
	if _, err = b.ChangeAddress("A1", changed, "", "actor", bookAt); !errors.Is(err, ErrInvalidChange) {
		t.Fatalf("change without a reason: %v", err)
	}
	if _, err = b.ChangeAddress("A1", office("HQ"), "same", "actor", bookAt); !errors.Is(err, ErrNothingChanged) {
		t.Fatalf("unchanged address: %v", err)
	}
	v, err = b.ChangeAddress("A1", changed, "office moved to the billing centre", "actor", bookAt)
	if err != nil || v.Version != 2 || v.Revision != 3 {
		t.Fatalf("change: v=%+v err=%v", v, err)
	}
	if _, ok := b.DefaultAddress(AddressOffice); ok {
		t.Fatal("an address that loses a purpose stops being its default")
	}
	if _, err = b.SetDefaultAddress(AddressOffice, "A1", "actor", bookAt); !errors.Is(err, ErrInvalidChange) {
		t.Fatalf("default for a purpose the address lacks: %v", err)
	}
	if _, err = b.SetDefaultAddress(AddressBilling, "A1", "actor", bookAt); err != nil {
		t.Fatal(err)
	}
	if _, err = b.SetDefaultAddress(AddressBilling, "A1", "actor", bookAt); !errors.Is(err, ErrNothingChanged) {
		t.Fatalf("same default again: %v", err)
	}
	v, err = b.DeactivateAddress("A1", "closed", "actor", bookAt)
	if err != nil || v.Version != 3 || v.Status != EntryInactive {
		t.Fatalf("deactivate: v=%+v err=%v", v, err)
	}
	if got := b.AddressDefaultsOf("A1"); len(got) != 0 {
		t.Fatalf("an inactive address is no default: %v", got)
	}
	if _, err = b.ChangeAddress("A1", office("HQ"), "reopen", "actor", bookAt); !errors.Is(err, ErrEntryNotActive) {
		t.Fatalf("change of an inactive address: %v", err)
	}
	if _, err = b.AddAddress("A3", office("HQ"), nil, "actor", bookAt); err != nil {
		t.Fatalf("the short code of an inactive address is free again: %v", err)
	}
	if _, err = b.DeactivateAddress("nope", "x", "actor", bookAt); !errors.Is(err, ErrUnknownAddress) {
		t.Fatalf("unknown address: %v", err)
	}
	if b.Revision() != 6 || len(b.Unsaved().Changes) != 6 || len(b.Pending()) != 6 {
		t.Fatalf("revision %d, %d changes, %d events: one of each per successful command", b.Revision(), len(b.Unsaved().Changes), len(b.Pending()))
	}
}

func TestAddressValidation(t *testing.T) {
	long := strings.Repeat("x", 101)
	for name, mutate := range map[string]func(*Address){
		"lower-case short code":     func(a *Address) { a.ShortCode = "hq" },
		"short code too long":       func(a *Address) { a.ShortCode = "ABCDEFGHIJKLMNOPQRSTU" },
		"no purpose":                func(a *Address) { a.Purposes = nil },
		"unknown purpose":           func(a *Address) { a.Purposes = []AddressPurpose{"HOME"} },
		"repeated purpose":          func(a *Address) { a.Purposes = []AddressPurpose{AddressOffice, AddressOffice} },
		"no name":                   func(a *Address) { a.Name = "  " },
		"no lines":                  func(a *Address) { a.Lines = nil },
		"five lines":                func(a *Address) { a.Lines = []string{"1", "2", "3", "4", "5"} },
		"empty line":                func(a *Address) { a.Lines = []string{"1", " "} },
		"line too long":             func(a *Address) { a.Lines = []string{long} },
		"line break in a line":      func(a *Address) { a.Lines = []string{"Road 1\nFloor 2"} },
		"no city":                   func(a *Address) { a.City = "" },
		"country not ISO 3166":      func(a *Address) { a.CountryCode = "cn" },
		"UN/LOCODE form":            func(a *Address) { a.UNLocode = "CNSH1" },
		"UN/LOCODE in another land": func(a *Address) { a.UNLocode = "USNYC" },
		"local without language":    func(a *Address) { a.Local.Language = "" },
		"local language form":       func(a *Address) { a.Local.Language = "Chinese" },
		"local without lines":       func(a *Address) { a.Local.Lines = nil },
		"postcode too long":         func(a *Address) { a.Postcode = strings.Repeat("9", 21) },
	} {
		a := office("HQ")
		mutate(&a)
		if _, err := a.normalized(); !errors.Is(err, ErrInvalidAddress) {
			t.Errorf("%s: err = %v, want ErrInvalidAddress", name, err)
		}
	}
	a := office(" HQ ")
	a.Name = "  Fixture Trading Co., Ltd.  "
	a.Local.Language = "zh-Hans"
	got, err := a.normalized()
	if err != nil || got.ShortCode != "HQ" || got.Name != "Fixture Trading Co., Ltd." {
		t.Fatalf("trimmed: %+v %v", got, err)
	}
	residential := warehouse("HOME")
	residential.Residential, residential.Purposes = true, []AddressPurpose{AddressDelivery}
	if _, err = residential.normalized(); err != nil {
		t.Fatalf("residential delivery address: %v", err)
	}
}

func TestContactValidation(t *testing.T) {
	for name, mutate := range map[string]func(*Contact){
		"no purpose":              func(c *Contact) { c.Purposes = nil },
		"address purpose":         func(c *Contact) { c.Purposes = []ContactPurpose{"OFFICE"} },
		"no name":                 func(c *Contact) { c.Name = "" },
		"no way to reach":         func(c *Contact) { c.Email = "" },
		"e-mail without domain":   func(c *Contact) { c.Email = "desk@" },
		"e-mail without dot":      func(c *Contact) { c.Email = "desk@localhost" },
		"e-mail with a space":     func(c *Contact) { c.Email = "desk a@example.test" },
		"phone with letters":      func(c *Contact) { c.Phone = "call me" },
		"phone with two digits":   func(c *Contact) { c.Phone = "+1 2" },
		"mobile with line breaks": func(c *Contact) { c.Mobile = "+86\n138" },
	} {
		c := bookingDesk()
		mutate(&c)
		if _, err := c.normalized(); !errors.Is(err, ErrInvalidContact) {
			t.Errorf("%s: err = %v, want ErrInvalidContact", name, err)
		}
	}
	c := Contact{Purposes: []ContactPurpose{ContactAccounts}, Name: "Test Person B", Phone: "+86 (21) 5555-0100", Mobile: "+86 139 0000 0000"}
	if _, err := c.normalized(); err != nil {
		t.Fatalf("phone and mobile without e-mail: %v", err)
	}
}

func TestEraseContactClearsEveryVersionAndDefault(t *testing.T) {
	b := newBook(t)
	if _, err := b.AddContact("C1", bookingDesk(), []ContactPurpose{ContactBooking}, "actor", bookAt); err != nil {
		t.Fatal(err)
	}
	changed := bookingDesk()
	changed.Phone = "+86 21 5555 0100"
	if _, err := b.ChangeContact("C1", changed, "added a phone", "actor", bookAt); err != nil {
		t.Fatal(err)
	}
	if id, ok := b.DefaultContact(ContactBooking); !ok || id != "C1" {
		t.Fatalf("default booking contact = %q %v", id, ok)
	}
	v, err := b.EraseContact("C1", "the person left the customer", "actor", bookAt)
	if err != nil || v.Status != EntryErased || !v.Erased || v.Version != 3 || v.Name != "" || v.Email != "" {
		t.Fatalf("erase: v=%+v err=%v", v, err)
	}
	if _, ok := b.DefaultContact(ContactBooking); ok {
		t.Fatal("an erased contact is no default")
	}
	u := b.Unsaved()
	for _, cv := range u.ContactVersions {
		if !cv.Erased || cv.Name != "" || cv.Phone != "" || cv.Mobile != "" || cv.Email != "" || len(cv.Purposes) == 0 {
			t.Fatalf("version %d keeps personal data or lost its purposes: %+v", cv.Version, cv)
		}
	}
	if !slices.Equal(u.Erased, []ContactID{"C1"}) {
		t.Fatalf("erased = %v", u.Erased)
	}
	if _, err = b.EraseContact("C1", "again", "actor", bookAt); !errors.Is(err, ErrNothingChanged) {
		t.Fatalf("erase twice: %v", err)
	}
	if _, err = b.ChangeContact("C1", bookingDesk(), "restore", "actor", bookAt); !errors.Is(err, ErrEntryNotActive) {
		t.Fatalf("change of an erased contact: %v", err)
	}
	for _, e := range b.Pending() {
		if e.SubjectID != "P" || e.EntryID != "C1" || e.Revision < 1 {
			t.Fatalf("event %+v must name the party, the contact and the revision", e)
		}
	}
}

func TestDeactivatedContactCanBeErased(t *testing.T) {
	b := newBook(t)
	if _, err := b.AddContact("C1", bookingDesk(), nil, "actor", bookAt); err != nil {
		t.Fatal(err)
	}
	if _, err := b.DeactivateContact("C1", "left", "actor", bookAt); err != nil {
		t.Fatal(err)
	}
	if _, err := b.SetDefaultContact(ContactBooking, "C1", "actor", bookAt); !errors.Is(err, ErrEntryNotActive) {
		t.Fatalf("an inactive contact cannot become a default: %v", err)
	}
	if v, err := b.EraseContact("C1", "retention period over", "actor", bookAt); err != nil || v.Version != 3 {
		t.Fatalf("erase inactive: %+v %v", v, err)
	}
}

func TestRestoreAddressBookChecksConsistency(t *testing.T) {
	a := AddressVersion{ID: "A1", Version: 1, Revision: 1, Status: EntryActive, Address: office("HQ")}
	b := AddressVersion{ID: "A2", Version: 1, Revision: 2, Status: EntryActive, Address: office("HQ")}
	if _, err := RestoreAddressBook("T", "P", 2, []AddressVersion{a, b}, nil, nil, nil); !errors.Is(err, ErrInconsistentBook) {
		t.Fatalf("two active HQ: %v", err)
	}
	if _, err := RestoreAddressBook("T", "P", 1, []AddressVersion{a}, nil, map[AddressPurpose]AddressID{AddressPickup: "A1"}, nil); !errors.Is(err, ErrInconsistentBook) {
		t.Fatalf("default for a purpose the address lacks: %v", err)
	}
	erased := ContactVersion{ID: "C1", Version: 2, Revision: 1, Status: EntryErased, Erased: true, Contact: Contact{Purposes: []ContactPurpose{ContactBooking}, Name: "kept"}}
	if _, err := RestoreAddressBook("T", "P", 1, nil, []ContactVersion{erased}, nil, nil); !errors.Is(err, ErrInconsistentBook) {
		t.Fatalf("erased contact with a name: %v", err)
	}
	if _, err := RestoreAddressBook("T", "P", 0, nil, nil, nil, nil); !errors.Is(err, ErrInconsistentBook) {
		t.Fatalf("stored book at revision 0: %v", err)
	}
	book, err := RestoreAddressBook("T", "P", 1, []AddressVersion{a}, nil, map[AddressPurpose]AddressID{AddressOffice: "A1"}, nil)
	if err != nil || book.LoadedRevision() != 1 || len(book.Unsaved().Changes) != 0 || len(book.Pending()) != 0 {
		t.Fatalf("restore: %v", err)
	}
}

func TestRequireMaintainable(t *testing.T) {
	for _, s := range []PartyStatus{PartyDraft, PartyActive, PartySuspended} {
		if err := RequireMaintainable(s); err != nil {
			t.Errorf("%s: %v", s, err)
		}
	}
	if err := RequireMaintainable(PartyArchived); !errors.Is(err, ErrPartyArchived) {
		t.Fatalf("archived: %v", err)
	}
}

// A model-based property test: random sequences of all nine commands keep the book's invariants, record one
// revision per successful command and survive a restore from their current state.
func TestAddressBookPropertyRandomCommands(t *testing.T) {
	addressPurposes := []AddressPurpose{AddressOffice, AddressPickup, AddressDelivery, AddressBilling}
	contactPurposes := []ContactPurpose{ContactBooking, ContactDocuments, ContactAccounts}
	for seed := uint64(1); seed <= 300; seed++ {
		r := rand.New(rand.NewPCG(seed, 7))
		b := newBook(t)
		ok := 0
		for step := 0; step < 40; step++ {
			aid := AddressID(fmt.Sprintf("A%d", r.IntN(5)))
			cid := ContactID(fmt.Sprintf("C%d", r.IntN(4)))
			addr := warehouse(fmt.Sprintf("S%d", r.IntN(4)))
			addr.Purposes = []AddressPurpose{addressPurposes[r.IntN(4)], addressPurposes[r.IntN(4)]}
			if addr.Purposes[0] == addr.Purposes[1] {
				addr.Purposes = addr.Purposes[:1]
			}
			contact := bookingDesk()
			contact.Purposes = []ContactPurpose{contactPurposes[r.IntN(3)]}
			contact.Email = fmt.Sprintf("desk-%d@example.test", r.IntN(3))
			var err error
			switch r.IntN(9) {
			case 0:
				_, err = b.AddAddress(aid, addr, addr.Purposes[:r.IntN(len(addr.Purposes)+1)], "actor", bookAt)
			case 1:
				_, err = b.ChangeAddress(aid, addr, "change", "actor", bookAt)
			case 2:
				_, err = b.DeactivateAddress(aid, "close", "actor", bookAt)
			case 3:
				_, err = b.SetDefaultAddress(addressPurposes[r.IntN(4)], aid, "actor", bookAt)
			case 4:
				_, err = b.AddContact(cid, contact, contact.Purposes[:r.IntN(2)], "actor", bookAt)
			case 5:
				_, err = b.ChangeContact(cid, contact, "change", "actor", bookAt)
			case 6:
				_, err = b.DeactivateContact(cid, "left", "actor", bookAt)
			case 7:
				_, err = b.EraseContact(cid, "erase", "actor", bookAt)
			case 8:
				_, err = b.SetDefaultContact(contactPurposes[r.IntN(3)], cid, "actor", bookAt)
			}
			if err == nil {
				ok++
			}
			checkBookInvariants(t, seed, b, ok)
		}
		restored, err := RestoreAddressBook("T", "P", max(b.Revision(), 1), b.Addresses(), b.Contacts(), defaultsOf(b, addressPurposes), contactDefaultsOf(b, contactPurposes))
		if b.Revision() > 0 && (err != nil || !slices.EqualFunc(restored.Addresses(), b.Addresses(), func(x, y AddressVersion) bool { return x.Address.equal(y.Address) && x.Version == y.Version })) {
			t.Fatalf("seed %d: restore of the current state: %v", seed, err)
		}
	}
}

func defaultsOf(b *AddressBook, purposes []AddressPurpose) map[AddressPurpose]AddressID {
	out := map[AddressPurpose]AddressID{}
	for _, p := range purposes {
		if id, ok := b.DefaultAddress(p); ok {
			out[p] = id
		}
	}
	return out
}

func contactDefaultsOf(b *AddressBook, purposes []ContactPurpose) map[ContactPurpose]ContactID {
	out := map[ContactPurpose]ContactID{}
	for _, p := range purposes {
		if id, ok := b.DefaultContact(p); ok {
			out[p] = id
		}
	}
	return out
}

func checkBookInvariants(t *testing.T, seed uint64, b *AddressBook, succeeded int) {
	t.Helper()
	u := b.Unsaved()
	if b.Revision() != int64(succeeded) || len(u.Changes) != succeeded || len(b.Pending()) != succeeded {
		t.Fatalf("seed %d: revision %d, %d changes, %d events after %d successful commands", seed, b.Revision(), len(u.Changes), len(b.Pending()), succeeded)
	}
	for i, c := range u.Changes {
		if c.Revision != int64(i+1) {
			t.Fatalf("seed %d: change %d has revision %d", seed, i, c.Revision)
		}
	}
	codes := map[string]bool{}
	for _, a := range b.Addresses() {
		if a.Status == EntryActive {
			if codes[a.ShortCode] {
				t.Fatalf("seed %d: two active addresses with short code %s", seed, a.ShortCode)
			}
			codes[a.ShortCode] = true
		}
		n := 0
		for _, v := range u.AddressVersions {
			if v.ID == a.ID {
				n++
				if v.Version != int64(n) {
					t.Fatalf("seed %d: address %s versions out of sequence", seed, a.ID)
				}
			}
		}
		if int64(n) != a.Version {
			t.Fatalf("seed %d: address %s is at version %d with %d versions", seed, a.ID, a.Version, n)
		}
		for _, p := range b.AddressDefaultsOf(a.ID) {
			if a.Status != EntryActive || !slices.Contains(a.Purposes, p) {
				t.Fatalf("seed %d: address %s is the %s default but %s for %v", seed, a.ID, p, a.Status, a.Purposes)
			}
		}
	}
	for _, c := range b.Contacts() {
		for _, p := range b.ContactDefaultsOf(c.ID) {
			if c.Status != EntryActive || !slices.Contains(c.Purposes, p) {
				t.Fatalf("seed %d: contact %s is the %s default but %s", seed, c.ID, p, c.Status)
			}
		}
		if c.Status == EntryErased {
			for _, v := range u.ContactVersions {
				if v.ID == c.ID && (!v.Erased || v.Name != "" || v.Email != "") {
					t.Fatalf("seed %d: erased contact %s keeps personal data in version %d", seed, c.ID, v.Version)
				}
			}
		}
	}
}
