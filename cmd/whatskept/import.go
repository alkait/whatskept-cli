package main

import (
	"fmt"

	"whatskept/internal/backup"
	"whatskept/internal/workspace"
)

func runImport(backupPath string) error {
	root, err := workspace.Find()
	if err != nil {
		return err
	}
	udid, err := backup.ReadUDID(backupPath)
	if err != nil {
		return err
	}
	s, err := workspace.Load(root)
	if err != nil {
		return err
	}

	switch {
	case s.UDID == "":
		s.UDID = udid
		if err := workspace.Save(root, s); err != nil {
			return err
		}
		fmt.Printf("workspace bound to device %s\n", udid)
	case s.UDID == udid:
		fmt.Printf("backup matches workspace device %s\n", udid)
	default:
		return fmt.Errorf("backup is from device %s but this workspace is bound to device %s", udid, s.UDID)
	}

	b, err := backup.Open(backupPath)
	if err != nil {
		return err
	}
	number, err := b.DetectNumber()
	if err != nil {
		return err
	}
	switch {
	case number == "":
		fmt.Println("warning: could not determine the WhatsApp account number from this backup")
	case s.WhatsAppNumber == "":
		s.WhatsAppNumber = number
		if err := workspace.Save(root, s); err != nil {
			return err
		}
		fmt.Printf("workspace bound to WhatsApp number %s\n", number)
	case s.WhatsAppNumber == number:
		fmt.Printf("backup matches workspace WhatsApp number %s\n", number)
	default:
		return fmt.Errorf("backup belongs to WhatsApp number %s but this workspace is bound to %s", number, s.WhatsAppNumber)
	}

	n, err := b.ExtractChatStorage(root)
	if err != nil {
		return err
	}
	fmt.Printf("extracted %s (%d bytes)\n", backup.ChatStorageName, n)

	stats, err := b.ExtractBlobs(root, func(line string) { fmt.Println(line) })
	if err != nil {
		return err
	}
	fmt.Printf("images: %d downloaded, %d missing, %d errors\n",
		stats.Images.Downloaded, stats.Images.Missing, stats.Images.Errors)
	fmt.Printf("voice notes: %d downloaded, %d missing, %d errors\n",
		stats.Voice.Downloaded, stats.Voice.Missing, stats.Voice.Errors)
	fmt.Printf("documents: %d downloaded, %d missing, %d errors, %d unsupported (non-PDF)\n",
		stats.Documents.Downloaded, stats.Documents.Missing, stats.Documents.Errors,
		stats.UnsupportedDocuments)

	fmt.Println("enrichment is not implemented yet")
	return nil
}
