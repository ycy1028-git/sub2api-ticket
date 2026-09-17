package tlsfingerprint

import (
	"testing"

	utls "github.com/refraction-networking/utls"
	"github.com/stretchr/testify/require"
)

func TestFilterSupportedUTLSCurvesDropsMLKEM(t *testing.T) {
	got := filterSupportedUTLSCurves([]utls.CurveID{
		4588,
		utls.X25519,
		utls.CurveP256,
		utls.CurveP384,
	})
	require.Equal(t, []utls.CurveID{utls.X25519, utls.CurveP256, utls.CurveP384}, got)
}

func TestCurvePreferencesFromProfileIgnoresMLKEM(t *testing.T) {
	profile := &Profile{Curves: []uint16{4588, 29, 23, 24}}
	got := curvePreferencesFromProfile(profile)
	require.Equal(t, []utls.CurveID{utls.X25519, utls.CurveP256, utls.CurveP384}, got)
}

func TestBuildClientHelloSpecStripsMLKEMFromKeyShare(t *testing.T) {
	profile := &Profile{
		Curves:         []uint16{4588, 29, 23, 24},
		KeyShareGroups: []uint16{4588},
	}
	spec := buildClientHelloSpecFromProfile(profile)
	require.NotNil(t, spec)
	for _, ext := range spec.Extensions {
		switch typed := ext.(type) {
		case *utls.SupportedCurvesExtension:
			require.NotContains(t, typed.Curves, utls.CurveID(4588))
			require.Contains(t, typed.Curves, utls.X25519)
		case *utls.KeyShareExtension:
			for _, share := range typed.KeyShares {
				require.NotEqual(t, utls.CurveID(4588), share.Group)
			}
			require.NotEmpty(t, typed.KeyShares)
			require.Equal(t, utls.X25519, typed.KeyShares[0].Group)
		}
	}
}
