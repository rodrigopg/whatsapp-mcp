package main

import (
	"fmt"
	"strconv"
	"strings"

	waProto "go.mau.fi/whatsmeow/binary/proto"
)

func locationText(latitude, longitude float64, live bool, label string) string {
	kind := "location"
	if live {
		kind = "live location"
	}
	// Plain decimals: %g switches to exponent form for coordinates near zero (1e-05).
	coord := func(v float64) string { return strconv.FormatFloat(v, 'f', -1, 64) }
	return strings.TrimSpace(fmt.Sprintf("[%s] %s,%s %s", kind, coord(latitude), coord(longitude), label))
}

func contactText(contact *waProto.ContactMessage) string {
	name := contact.GetDisplayName()
	var phones []string
	card := strings.ReplaceAll(contact.GetVcard(), "\r\n", "\n")
	card = strings.ReplaceAll(strings.ReplaceAll(card, "\n ", ""), "\n\t", "")
	for _, line := range strings.Split(card, "\n") {
		field, value, ok := strings.Cut(line, ":")
		if !ok {
			continue
		}
		field, _, _ = strings.Cut(field, ";")
		if i := strings.LastIndex(field, "."); i >= 0 {
			field = field[i+1:]
		}
		switch strings.ToUpper(field) {
		case "FN":
			if name == "" {
				name = strings.NewReplacer(`\n`, " ", `\N`, " ", `\,`, ",", `\;`, ";", `\\`, `\`).Replace(value)
			}
		case "TEL":
			phones = append(phones, strings.TrimPrefix(value, "tel:"))
		}
	}
	return strings.TrimSpace("[contact] " + strings.TrimSpace(name+" "+strings.Join(phones, ", ")))
}
