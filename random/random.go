package random

import (
	"math/rand"
	"strings"
	"time"
)

// Password creates a random password
func Password(minLength, maxLength int) string {
	if maxLength < minLength {
		maxLength = minLength
	}
	if maxLength == 0 {
		maxLength = 32
	}
	if minLength == 0 {
		minLength = 8
	}

	// Seed the random number generator
	rand.Seed(time.Now().UnixNano())
	chars := []rune("ABCDEFGHIJKLMNOPQRSTUVWXYZ" +
		"abcdefghijklmnopqrstuvwxyz" +
		"0123456789" +
		"!$%&()=?")
	var b strings.Builder
	for i := 0; i < maxLength; i++ {
		b.WriteRune(chars[rand.Intn(len(chars))])
	}
	str := b.String() // E.g. "ExcbsVQs"

	length := maxLength
	if maxLength > minLength {
		length = rand.Intn(maxLength-minLength) + minLength
	}
	str = str[:length]

	return str
}

// Number creates a random number
func Number(min, max int) int {
	if max < min {
		max = min
	}
	if max == 0 {
		max = 100
	}
	if min == 0 {
		min = 1
	}
	rand.Seed(time.Now().UnixNano())

	r := rand.Intn(max)

	if max > min {
		r = rand.Intn(max-min) + min
	}

	return r
}

// Bool creates a random boolean
func Bool() bool {
	rand.Seed(time.Now().UnixNano())
	return rand.Intn(2) == 1
}
