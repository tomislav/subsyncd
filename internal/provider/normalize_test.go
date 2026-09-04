package provider

import "testing"

func TestNormalizePopularityIsBoundedAndMonotonic(t *testing.T) {
	values := []float64{NormalizePopularity(-1), NormalizePopularity(0), NormalizePopularity(9), NormalizePopularity(99), NormalizePopularity(999), NormalizePopularity(9999), NormalizePopularity(1_000_000)}
	if values[0] != 0 || values[1] != 0 || values[len(values)-1] != 1 {
		t.Fatalf("boundary values = %#v", values)
	}
	for index := 1; index < len(values); index++ {
		if values[index] < values[index-1] || values[index] < 0 || values[index] > 1 {
			t.Fatalf("popularity values are not bounded and monotonic: %#v", values)
		}
	}
}
