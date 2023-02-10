package random

import (
	"testing"

	"github.com/stretchr/testify/assert"
)

func TestPassword(t *testing.T) {
	got := Password(251, 251)
	assert.Equal(t, 251, len(got))
}
