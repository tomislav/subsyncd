package provider

import "math"

// NormalizePopularity maps provider download counts to [0,1] on a logarithmic
// scale. Four orders of magnitude reach saturation so a viral subtitle cannot
// outweigh release identity evidence.
func NormalizePopularity(downloadCount int64) float64 {
	if downloadCount <= 0 {
		return 0
	}
	return min(math.Log10(float64(downloadCount)+1)/4, 1)
}
