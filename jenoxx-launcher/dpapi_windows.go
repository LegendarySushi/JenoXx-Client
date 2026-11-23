//go:build windows

package main

import (
	"errors"
	"syscall"
	"unsafe"
)

// Windows DPAPI: verschluesselt Daten an das aktuelle Benutzerkonto.

type dataBlob struct {
	cbData uint32
	pbData *byte
}

var (
	crypt32       = syscall.NewLazyDLL("crypt32.dll")
	kernel32      = syscall.NewLazyDLL("kernel32.dll")
	procProtect   = crypt32.NewProc("CryptProtectData")
	procUnprotect = crypt32.NewProc("CryptUnprotectData")
	procLocalFree = kernel32.NewProc("LocalFree")
)

func newBlob(d []byte) *dataBlob {
	if len(d) == 0 {
		return &dataBlob{}
	}
	return &dataBlob{cbData: uint32(len(d)), pbData: &d[0]}
}

func (b *dataBlob) bytes() []byte {
	out := make([]byte, b.cbData)
	if b.cbData > 0 {
		copy(out, unsafe.Slice(b.pbData, b.cbData))
	}
	return out
}

func protectData(plain []byte) ([]byte, error) {
	var out dataBlob
	r, _, err := procProtect.Call(uintptr(unsafe.Pointer(newBlob(plain))), 0, 0, 0, 0, 0, uintptr(unsafe.Pointer(&out)))
	if r == 0 {
		return nil, errors.New("CryptProtectData: " + err.Error())
	}
	defer procLocalFree.Call(uintptr(unsafe.Pointer(out.pbData)))
	return out.bytes(), nil
}

func unprotectData(enc []byte) ([]byte, error) {
	var out dataBlob
	r, _, err := procUnprotect.Call(uintptr(unsafe.Pointer(newBlob(enc))), 0, 0, 0, 0, 0, uintptr(unsafe.Pointer(&out)))
	if r == 0 {
		return nil, errors.New("CryptUnprotectData: " + err.Error())
	}
	defer procLocalFree.Call(uintptr(unsafe.Pointer(out.pbData)))
	return out.bytes(), nil
}
