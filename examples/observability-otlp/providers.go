package main

import "encoding/base64"

func basicAuthorization(publicKey, secretKey string) string {
	return "Basic " + base64.StdEncoding.EncodeToString([]byte(publicKey+":"+secretKey))
}

func must[T any](value T, err error) T {
	if err != nil {
		panic(err)
	}
	return value
}

func mustNoError(err error) {
	if err != nil {
		panic(err)
	}
}
