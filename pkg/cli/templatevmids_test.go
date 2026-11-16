package cli

import (
	"strings"
	"testing"
)

func TestTemplateBuildsTakeTheVMIDRangeOfTheirFlag(t *testing.T) {
	t.Parallel()
	b := &builderFlags{vmIDRange: "10000-10999"}
	if err := b.idRange(); err != nil || b.builder.FirstVMID != 10000 || b.builder.LastVMID != 10999 {
		t.Errorf("the range was read as %d-%d, %v", b.builder.FirstVMID, b.builder.LastVMID, err)
	}
	none := &builderFlags{}
	if err := none.idRange(); err != nil || none.builder.LastVMID != 0 {
		t.Errorf("without the flag the builder has %d-%d, %v", none.builder.FirstVMID, none.builder.LastVMID, err)
	}
	for _, bad := range []string{"2000-1000", "99-1000", "10000", "a-b"} {
		if err := (&builderFlags{vmIDRange: bad}).idRange(); err == nil || !strings.Contains(err.Error(), "--vm-id-range") {
			t.Errorf("%q: %v", bad, err)
		}
	}
}
