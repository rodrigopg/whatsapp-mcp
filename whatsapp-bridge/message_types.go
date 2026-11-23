package main

import (
	"fmt"
	"strings"

	waProto "go.mau.fi/whatsmeow/binary/proto"
)

func locationText(latitude, longitude float64, live bool, label string) string {
	kind := "location"
	if live {
		kind = "live location"
	}
	return strings.TrimSpace(fmt.Sprintf("[%s] %g,%g %s", kind, latitude, longitude, label))
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
