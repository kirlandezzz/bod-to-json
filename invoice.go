// Package bod parses an Infor SyncSupplierInvoice BOD and posts it as JSON.
package bod

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/xml"
	"errors"
	"fmt"
	"io"
	"math/big"
	"strings"
	"time"
)

// Amount is an element like <TotalAmount currencyID="USD">110</TotalAmount>.
// The value stays a decimal string: float64 cannot represent amounts such as
// 0.10 exactly, so money is never converted to a float.
type Amount struct {
	Value    string `xml:",chardata" json:"value"`
	Currency string `xml:"currencyID,attr" json:"currency"`
}

// rat returns the amount as an exact fraction.
func (a Amount) rat() (*big.Rat, bool) {
	return new(big.Rat).SetString(strings.TrimSpace(a.Value))
}

// Action is <ActionExpression actionCode="Add">: Add, Change, Replace or Delete.
// encoding/xml cannot read an attribute at the end of an a>b>c path, so the
// element gets its own type.
type Action struct {
	Code string `xml:"actionCode,attr" json:"code"`
}

type Line struct {
	Number string `xml:"LineNumber" json:"number"`
	Item   string `xml:"Item>ItemID>ID" json:"item,omitempty"`
	Amount Amount `xml:"ExtendedAmount" json:"amount"`
}

// Invoice holds the fields we need from the BOD. Paths are relative to the
// root element; the XMLName tag makes Decode reject any other root or namespace.
type Invoice struct {
	XMLName xml.Name `xml:"http://schema.infor.com/InforOAGIS/2 SyncSupplierInvoice" json:"-"`

	// Where the message comes from.
	Sender           string `xml:"ApplicationArea>Sender>LogicalID" json:"sender,omitempty"`
	Tenant           string `xml:"DataArea>Sync>TenantID" json:"tenant,omitempty"`
	AccountingEntity string `xml:"DataArea>Sync>AccountingEntityID" json:"accountingEntity,omitempty"`
	Action           Action `xml:"DataArea>Sync>ActionCriteria>ActionExpression" json:"action"`

	// The invoice itself.
	ID            string `xml:"DataArea>SupplierInvoice>SupplierInvoiceHeader>DocumentID>ID" json:"id"`
	Supplier      string `xml:"DataArea>SupplierInvoice>SupplierInvoiceHeader>SupplierParty>PartyIDs>ID" json:"supplier"`
	Date          string `xml:"DataArea>SupplierInvoice>SupplierInvoiceHeader>DocumentDateTime" json:"date"`
	PurchaseOrder string `xml:"DataArea>SupplierInvoice>SupplierInvoiceHeader>PurchaseOrderReference>DocumentID>ID" json:"purchaseOrder,omitempty"`
	Net           Amount `xml:"DataArea>SupplierInvoice>SupplierInvoiceHeader>ExtendedAmount" json:"net"`
	Total         Amount `xml:"DataArea>SupplierInvoice>SupplierInvoiceHeader>TotalAmount" json:"total"`
	Lines         []Line `xml:"DataArea>SupplierInvoice>SupplierInvoiceLine" json:"lines"`
}

// ParseInvoice decodes a SyncSupplierInvoice BOD.
func ParseInvoice(r io.Reader) (Invoice, error) {
	var inv Invoice
	if err := xml.NewDecoder(r).Decode(&inv); err != nil {
		return Invoice{}, fmt.Errorf("parse BOD: %w", err)
	}
	return inv, nil
}

var actionCodes = map[string]bool{"Add": true, "Change": true, "Replace": true, "Delete": true}

// Validate reports every problem at once, so a person fixing the data sees
// the full list instead of one error per attempt.
func (inv Invoice) Validate() error {
	var errs []error
	fail := func(format string, args ...any) { errs = append(errs, fmt.Errorf(format, args...)) }

	if !actionCodes[inv.Action.Code] {
		fail("action code %q is not Add, Change, Replace or Delete", inv.Action.Code)
	}
	if strings.TrimSpace(inv.ID) == "" {
		fail("invoice ID is empty")
	}
	if strings.TrimSpace(inv.Supplier) == "" {
		fail("supplier is empty")
	}
	if _, err := time.Parse(time.RFC3339, strings.TrimSpace(inv.Date)); err != nil {
		fail("document date %q is not a timestamp like 2019-01-13T06:56:52Z", inv.Date)
	}

	currency := strings.TrimSpace(inv.Total.Currency)
	if currency == "" {
		fail("currency is empty")
	}
	total, ok := inv.Total.rat()
	switch {
	case !ok:
		fail("total %q is not a number", inv.Total.Value)
	case total.Sign() <= 0:
		fail("total %s is not positive", inv.Total.Value)
	}

	// Lines must be in the invoice currency and add up to the net amount.
	sum, sumOK := new(big.Rat), true
	for _, l := range inv.Lines {
		amount, ok := l.Amount.rat()
		if !ok {
			fail("line %s: amount %q is not a number", l.Number, l.Amount.Value)
			sumOK = false
			continue
		}
		if l.Amount.Currency != currency {
			fail("line %s: currency %q differs from invoice currency %q", l.Number, l.Amount.Currency, currency)
		}
		sum.Add(sum, amount)
	}
	if net, ok := inv.Net.rat(); ok && sumOK && len(inv.Lines) > 0 && sum.Cmp(net) != 0 {
		fail("lines add up to %s but the net amount is %s", sum.FloatString(2), net.FloatString(2))
	}
	return errors.Join(errs...)
}

// IdempotencyKey identifies the business document, not the message: the same
// invoice sent twice gets the same key, so the receiver can drop the duplicate.
// Tenant and accounting entity are part of it because two companies can
// receive the same invoice number from the same supplier.
func (inv Invoice) IdempotencyKey() string {
	parts := []string{
		strings.ToUpper(strings.TrimSpace(inv.Tenant)),
		strings.ToUpper(strings.TrimSpace(inv.AccountingEntity)),
		strings.ToUpper(strings.TrimSpace(inv.Supplier)),
		strings.ToUpper(strings.TrimSpace(inv.ID)),
		strings.TrimSpace(inv.Date),
	}
	sum := sha256.Sum256([]byte(strings.Join(parts, "\x1f")))
	return hex.EncodeToString(sum[:])
}
