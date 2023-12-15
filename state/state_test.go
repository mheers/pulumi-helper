package state

import (
	"testing"

	"github.com/stretchr/testify/require"
)

func TestList(t *testing.T) {
	l, err := List()
	require.NoError(t, err)
	require.NotEmpty(t, l)
}

func TestOutputs(t *testing.T) {
	state, err := GetState("az-azure-kubernetes-service-webm")
	require.NoError(t, err)
	require.NotEmpty(t, state)

	outputs, err := state.Outputs()
	require.NoError(t, err)
	require.NotEmpty(t, outputs)
}

func TestGetOutput(t *testing.T) {
	state, err := GetState("az-azure-kubernetes-service-webm")
	require.NoError(t, err)
	require.NotEmpty(t, state)

	outputs := []string{}
	err = state.GetOutput("dnsZone Nameservers", &outputs)
	require.NoError(t, err)
	require.NotEmpty(t, outputs)
	require.Len(t, outputs, 4)
}
