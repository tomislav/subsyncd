package catalog

import "testing"

func TestStreamingServiceFromSceneNameRequiresReleaseEvidence(t *testing.T) {
	for _, test := range []struct{ raw, want string }{
		{"Example.Movie.2020.1080p.AMZN.WEB-DL-GROUP", "amazon"},
		{"Example.Show.S01E02.NF.1080p.WEB-DL-GROUP", "netflix"},
		{"Example_Show_S01E02_1080p_NF_WEBRip_GROUP", "netflix"},
		{"Example.Show.S01.COMPLETE.1080p.NF.WEB-DL-GROUP", "netflix"},
		{"Example.Movie.2049.2017.1080p.MAX.WEB-DL-GROUP", "max"},
		{"Max.2015.1080p.WEB-DL-GROUP", ""},
		{"Amazon.2020.1080p.WEB-DL-GROUP", ""},
		{"Hulu.Show.S01E02.1080p.WEB-DL-GROUP", ""},
		{"Example.Movie.2020.1080p.NF.BluRay-GROUP", ""},
		{"Example.Movie.2020.1080p.WEB-DL-GROUP", ""},
		{"NF.1080p.WEB-DL-GROUP", ""},
		{"", ""},
	} {
		t.Run(test.raw, func(t *testing.T) {
			if got := streamingServiceFromSceneName(test.raw); got != test.want {
				t.Fatalf("service = %q, want %q", got, test.want)
			}
		})
	}
}
