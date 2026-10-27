package install

import (
	"context"
	"strings"
	"testing"
)

func TestPersist(t *testing.T) {
	table := `{"partitiontable":{"label":"gpt","device":"/dev/sda","sectorsize":512,"partitions":[
		{"node":"/dev/sda1","start":64,"size":6000000},{"node":"/dev/sda2","start":6000064,"size":20000}]}}`
	var calls []Cmd
	run := func(_ context.Context, c Cmd) (string, error) {
		calls = append(calls, c)
		if c.String() == "sfdisk --json /dev/sda" {
			return table, nil
		}
		return "", nil
	}
	part, err := Persist(context.Background(), run, "/dev/sda", 64e9)
	if err != nil || part != "/dev/sda3" {
		t.Fatalf("Persist = %q, %v", part, err)
	}
	var got []string
	for _, c := range calls {
		got = append(got, c.String())
	}
	want := []string{
		"blkid -L SIBYLPERSIST",
		"sfdisk --json /dev/sda",
		"sfdisk --relocate gpt-bak-std --no-reread /dev/sda",
		"sfdisk --append --no-reread --no-tell-kernel /dev/sda",
		"partx --add --nr 3 /dev/sda",
		"udevadm settle",
		"mkfs.xfs -f -L SIBYLPERSIST /dev/sda3",
	}
	if strings.Join(got, "\n") != strings.Join(want, "\n") {
		t.Fatalf("calls:\n%s", strings.Join(got, "\n"))
	}

	if _, err := Persist(context.Background(), run, "/dev/sda", 8e9); err == nil {
		t.Fatal("made a partition on an 8 GB USB")
	}
	exists := func(_ context.Context, c Cmd) (string, error) { return "/dev/sda3\n", nil }
	if _, err := Persist(context.Background(), exists, "/dev/sda", 64e9); err == nil {
		t.Fatal("made a second persistence partition")
	}
}
