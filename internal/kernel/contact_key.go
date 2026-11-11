package kernel

import (
	"crypto/sha256"
	"encoding/hex"
	"regexp"
)

// A contact key names a person whose contact details a document or booking carries, without carrying them (privacy
// decision P5): ORG:<party>:<contact id> for a contact of ORG's address book (contact ids are a party's own),
// COM:<order>:<role>:<party version> for a contact
// given on a COM order, or SHA:<SHA-256 of either form> when an id holds characters outside the grammar (such as an
// e-mail address used as an id), so that no key ever carries what could be a name, phone or e-mail. DOC and BOOK
// build keys with ContactKey; ORG's ContactTransferRegister v1 counts persons by them.
var contactKey = regexp.MustCompile(`^(ORG:[A-Za-z0-9._-]{1,128}:[A-Za-z0-9._-]{1,128}|COM:[A-Za-z0-9._-]{1,128}:(SHIPPER|CONSIGNEE|NOTIFY_PARTY):[1-9][0-9]{0,9}|SHA:[0-9a-f]{64})$`)

// ORGContactKey is the key of a contact of a party in ORG's address book.
func ORGContactKey(partyID, contactID string) string {
	return ContactKey("ORG:" + partyID + ":" + contactID)
}

// OrderContactKey is the key of a contact given on a COM order for a party role at a party version.
func OrderContactKey(orderID, role string, partyVersion int64) string {
	return ContactKey("COM:" + orderID + ":" + role + ":" + itoa(partyVersion))
}

// ContactKey returns form if it is a valid key, else its digest form.
func ContactKey(form string) string {
	if contactKey.MatchString(form) && form[:4] != "SHA:" {
		return form
	}
	sum := sha256.Sum256([]byte(form))
	return "SHA:" + hex.EncodeToString(sum[:])
}

// ValidContactKey reports whether k follows the contact key grammar.
func ValidContactKey(k string) bool { return contactKey.MatchString(k) }

func itoa(n int64) string {
	if n <= 0 {
		return "0"
	}
	var b [20]byte
	i := len(b)
	for n > 0 {
		i--
		b[i] = byte('0' + n%10)
		n /= 10
	}
	return string(b[i:])
}
