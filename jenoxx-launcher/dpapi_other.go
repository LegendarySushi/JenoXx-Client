//go:build !windows

package main

// Ausserhalb von Windows (nur Entwicklung/Tests): keine Verschluesselung.
func protectData(plain []byte) ([]byte, error) { return plain, nil }
func unprotectData(enc []byte) ([]byte, error) { return enc, nil }
