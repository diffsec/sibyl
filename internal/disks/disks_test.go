package disks

import "testing"

const lsblkJSON = `{"blockdevices":[
 {"name":"nvme0n1","path":"/dev/nvme0n1","size":1000204886016,"model":"CT1000P3SSD8","serial":"2318E6D24DDE","type":"disk","tran":"nvme","rm":false,"ro":false,"mountpoints":[null],
  "children":[{"name":"nvme0n1p1","path":"/dev/nvme0n1p1","size":629145600,"model":null,"serial":null,"type":"part","tran":"nvme","rm":false,"ro":false,"mountpoints":[null]}]},
 {"name":"sda","path":"/dev/sda","size":64023257088,"model":"Flash Drive","serial":"0374","type":"disk","tran":"usb","rm":true,"ro":false,"mountpoints":[],
  "children":[{"name":"sda1","path":"/dev/sda1","size":3000000000,"type":"part","rm":true,"ro":false,"mountpoints":["/run/initramfs/live"]}]},
 {"name":"sdb","path":"/dev/sdb","size":2000398934016,"model":"WDC","serial":"WD-1","type":"disk","tran":"sata","rm":false,"ro":false,"mountpoints":[],
  "children":[{"name":"sdb1","path":"/dev/sdb1","size":2000398934016,"type":"part","rm":false,"ro":false,"mountpoints":["/var/mnt/models-1"]}]},
 {"name":"sdc","path":"/dev/sdc","size":32000000000,"model":"Small","serial":"S","type":"disk","tran":"sata","rm":false,"ro":false,"mountpoints":[]},
 {"name":"sdd","path":"/dev/sdd","size":2000398934016,"model":"WDC","serial":"WD-2","type":"disk","tran":"sata","rm":false,"ro":false,"mountpoints":[],
  "children":[{"name":"sdd2","path":"/dev/sdd2","size":17179869184,"type":"part","rm":false,"ro":false,"mountpoints":["[SWAP]"]}]},
 {"name":"zram0","path":"/dev/zram0","size":8589934592,"type":"disk","rm":false,"ro":false,"mountpoints":["[SWAP]"]}
]}`

func TestParseDisks(t *testing.T) {
	disks, err := Parse([]byte(lsblkJSON), []string{"/dev/sda"}, 64<<30)
	if err != nil {
		t.Fatal(err)
	}
	want := map[string]string{
		"/dev/nvme0n1": "",
		"/dev/sda":     "the running system boots from it",
		"/dev/sdb":     "in use: mounted at /var/mnt/models-1",
		"/dev/sdc":     "smaller than 64 GiB",
		"/dev/sdd":     "in use as swap: /dev/sdd2",
	}
	if len(disks) != len(want) {
		t.Fatalf("disks %+v", disks)
	}
	for _, d := range disks {
		if r, ok := want[d.Path]; !ok || r != d.Reason {
			t.Errorf("%s: reason %q, want %q", d.Path, d.Reason, r)
		}
	}
	if l := disks[0].Label(); l != "/dev/nvme0n1  1000 GB  CT1000P3SSD8  serial 2318E6D24DDE  nvme" {
		t.Errorf("label %q", l)
	}
}

func TestPartition(t *testing.T) {
	for path, want := range map[string]string{"/dev/sda": "/dev/sda3", "/dev/nvme0n1": "/dev/nvme0n1p3", "/dev/vda": "/dev/vda3"} {
		if got := (Disk{Path: path}).Partition(3); got != want {
			t.Errorf("%s: %s, want %s", path, got, want)
		}
	}
}
