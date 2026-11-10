package kernel

// AccessPurpose is why a caller reads contacts, which are personal data. Every read of contacts states one,
// and the context that holds the contacts records it with the read (privacy assessment V1.2, section 8;
// decision P7).
type AccessPurpose string

const (
	PurposeDocuments          AccessPurpose = "DOCUMENTS"            // issuing transport documents
	PurposeBooking            AccessPurpose = "BOOKING"              // booking with carriers
	PurposeOperations         AccessPurpose = "OPERATIONS"           // pickup, delivery and exceptions
	PurposeAccounts           AccessPurpose = "ACCOUNTS"             // billing and collection
	PurposeOrderParty         AccessPurpose = "ORDER_PARTY"          // carrying a contact onto an order
	PurposeCustomsAudit       AccessPurpose = "CUSTOMS_AUDIT"        // answering a customs audit
	PurposeLegalClaim         AccessPurpose = "LEGAL_CLAIM"          // establishing or defending a legal claim
	PurposeDataSubjectRequest AccessPurpose = "DATA_SUBJECT_REQUEST" // answering the person's own request
)

// Valid reports whether p is one of the purposes above.
func (p AccessPurpose) Valid() bool {
	switch p {
	case PurposeDocuments, PurposeBooking, PurposeOperations, PurposeAccounts, PurposeOrderParty,
		PurposeCustomsAudit, PurposeLegalClaim, PurposeDataSubjectRequest:
		return true
	}
	return false
}

// ForRetainedData reports whether p may read contacts that are kept only because the law requires it, such as
// those of a closed or cancelled order (PIPL article 47; decision P1): a customs audit, a legal claim or the
// person's own request.
func (p AccessPurpose) ForRetainedData() bool {
	return p == PurposeCustomsAudit || p == PurposeLegalClaim || p == PurposeDataSubjectRequest
}
