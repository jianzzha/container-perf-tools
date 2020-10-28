package main

import (
	"flag"
	"fmt"
	"io/ioutil"
	"log"
	"os"
	"os/exec"
	"os/signal"
	"regexp"
	"strconv"
	"strings"
	"syscall"
	"time"

	expect "github.com/google/goexpect"
	"github.com/lithammer/shortuuid"
	"k8s.io/kubernetes/pkg/kubelet/cm/cpuset"
)

const (
	startTimeout = 60 * time.Second
	cmdTimeout   = 1 * time.Second
	pciDeviceDir = "/sys/bus/pci/devices/"
	pciDriverDir = "/sys/bus/pci/drivers/"
)

var (
	promptRE = regexp.MustCompile(`testpmd>`)
)

type testpmd struct {
	fwdMode    string
	running    bool
	filePrefix string
	e          *expect.GExpect
}

func (t *testpmd) init(pci pciArray, queues int, ring int, testpmdPath string) error {
	ports := len(pci)
	nPmd := ports * queues
	// one extra core for mgmt in addition to the pmd
	nCores := nPmd + 1
	cset := getProcCpuset()
	if nCores > cset.Size() {
		log.Fatal("insufficient cores!")
	}
	clist := intToString(cset.ToSlice()[:nCores], ",")
	t.filePrefix = shortuuid.New()
	// use 1024,1024 so no need to worry about numa node
	cmd := fmt.Sprintf("%s --socket-mem 1024,1024 -n 4 --proc-type auto", testpmdPath)
	cmd = fmt.Sprintf("%s -l %s", cmd, clist)
	// use a unique file-prefix
	cmd = fmt.Sprintf("%s --file-prefix %s", cmd, t.filePrefix)
	// add each pci address
	for _, p := range pci {
		cmd = fmt.Sprintf("%s -w %s", cmd, p)
	}
	// this has to go first before the rest
	cmd = fmt.Sprintf("%s -- -i", cmd)
	cmd = fmt.Sprintf("%s --nb-cores=%d", cmd, nPmd)
	cmd = fmt.Sprintf("%s --nb-ports=%d", cmd, ports)
	cmd = fmt.Sprintf("%s --portmask=%s", cmd, portMask(ports))
	cmd = fmt.Sprintf("%s --rxq=%d", cmd, queues)
	cmd = fmt.Sprintf("%s --txq=%d", cmd, queues)
	cmd = fmt.Sprintf("%s --rxd=%d", cmd, ring)
	cmd = fmt.Sprintf("%s --txd=%d", cmd, ring)
	log.Printf("cmd: %s", cmd)
	e, _, err := expect.Spawn(cmd, startTimeout)
	if err != nil {
		log.Fatal(err)
	}
	t.e = e
	if _, _, err := t.e.Expect(promptRE, cmdTimeout); err != nil {
		return err
	}
	return nil
}

func (t *testpmd) stop() error {
	t.e.Close()
	return nil
}

func (t *testpmd) runCmd(cmd string) (string, error) {
	t.e.Send(cmd + "\n")
	output, _, err := t.e.Expect(promptRE, cmdTimeout)
	return output, err
}

func (t *testpmd) setFwdMode(mode string) error {
	if t.running {
		if _, err := t.runCmd("stop"); err != nil {
			return err
		}
		t.running = false
	}
	if _, err := t.runCmd("set fwd " + mode); err != nil {
		return err
	}
	if _, err := t.runCmd("start"); err != nil {
		return err
	}
	t.running = true
	return nil
}

func (t *testpmd) icmpMode() error {
	return t.setFwdMode("icmpecho")
}

func (t *testpmd) ioMode() error {
	return t.setFwdMode("io")
}

func (t *testpmd) macMode() error {
	return t.setFwdMode("mac")
}

func (t *testpmd) getMacAddress(pci string) (string, error) {
	output, err := t.runCmd("show device info " + pci)
	if err != nil {
		return "", err
	}
	macRe := regexp.MustCompile(`MAC address:\s*(([0-9a-fA-F]{2}:){5}[0-9a-fA-F]{2})`)
	mac := macRe.FindStringSubmatch(output)[1]
	if strings.Contains(mac, ":") {
		return mac, nil
	}
	return "", fmt.Errorf("couldn't get mac address for pci slot %s", pci)
}

func intToString(a []int, delim string) string {
	b := ""
	for _, v := range a {
		if len(b) > 0 {
			b += delim
		}
		b += strconv.Itoa(v)
	}
	return b
}

func portMask(ports int) string {
	var a uint8
	for i := 0; i < ports; i++ {
		a = a | (1 << i)
	}
	return fmt.Sprintf("%#x", a)
}

type pciArray []string

//pci info, 1:1 map to pci array
type pciInfo struct {
	//driver previously
	driverPre string
	//driver current
	driverCur string
	//kernel driver
	//was this a kernel port before
	wasKernelPort bool
	//kernel driver
	kmod   string
	vendor string
	device string
}

//var pciRecord map[string]pciInfo

func (p *pciArray) String() string {
	return strings.Join(*p, " ")
}

func (p *pciArray) Set(value string) error {
	// todo: validate pci address exists
	// normalize pci address to start with 0000: prefix
	pci := value
	if !strings.HasPrefix(pci, "0000:") {
		pci = "0000:" + pci
	}
	if _, err := os.Stat(pciDeviceDir + pci); os.IsNotExist(err) {
		log.Fatalf("invalid pci %s", value)
		return fmt.Errorf("invalid pci %s", value)
	}
	*p = append(*p, pci)
	return nil
}

func getProcCpuset() cpuset.CPUSet {
	content, err := ioutil.ReadFile("/proc/self/status")
	if err != nil {
		panic(err)
	}
	r := regexp.MustCompile(`Cpus_allowed_list:\s*([0-9,-]*)\r?\n`)
	cpus := r.FindStringSubmatch(string(content))[1]
	return cpuset.MustParse(cpus)
}

func isKernelDevice(pci string) bool {
	if _, err := os.Stat(pciDeviceDir + pci + "/net"); !os.IsNotExist(err) {
		log.Printf("isKernelDevice: %s is kernel port", pci)
		return true
	}
	log.Printf("isKernelDevice: %s not kernel port", pci)
	return false
}

func isDeviceBound(pci string) (bool, string) {
	driverPath := pciDeviceDir + pci + "/driver"
	if _, err := os.Stat(driverPath); !os.IsNotExist(err) {
		cmd := exec.Command("readlink", "-f", driverPath)
		out, _ := cmd.Output()
		d := strings.Split(strings.TrimSpace(string(out)), "/")
		driver := d[len(d)-1]
		log.Printf("isDeviceBound: %s is bound to %s", pci, driver)
		return true, driver
	}
	log.Printf("isDeviceBound: %s", driverPath)
	log.Printf("isDeviceBound: %s not bound", pci)
	return false, ""
}

func unbind(pci string) error {
	driverPath := pciDeviceDir + pci + "/driver"
	cmd := exec.Command("readlink", "-f", driverPath)
	out, _ := cmd.Output()
	log.Printf("unbind: echo %s > %s\n", pci, strings.TrimSpace(string(out))+"/unbind")
	return ioutil.WriteFile(strings.TrimSpace(string(out))+"/unbind", []byte(pci), 0200)
}

func bind(pci string, driver string) error {
	driverPath := pciDriverDir + driver
	log.Printf("bind: echo %s > %s\n", pci, driverPath+"/bind")
	return ioutil.WriteFile(driverPath+"/bind", []byte(pci), 0200)
}

func pciNewID(vendor string, device string, driver string) error {
	newIDPath := pciDriverDir + driver + "/new_id"
	log.Printf("pciNewID: echo %s %s > %s\n", vendor, device, newIDPath)
	return ioutil.WriteFile(newIDPath, []byte(vendor+" "+device), 0200)
}

func pciRemoveID(vendor string, device string, driver string) error {
	removeIDPath := pciDriverDir + driver + "/remove_id"
	log.Printf("pciRemoveID: echo %s %s > %s\n", vendor, device, removeIDPath)
	return ioutil.WriteFile(removeIDPath, []byte(vendor+" "+device), 0200)
}

type deviceDriver struct {
	device string
	vendor string
	kmod   string
}

var driverArray = []deviceDriver{
	{
		vendor: "0x8086",
		device: "0x158b",
		kmod:   "i40e",
	},
}

func getDriverFromDeviceVendor(vendor string, device string) (string, error) {
	for _, d := range driverArray {
		if d.vendor == vendor && d.device == device {
			return d.kmod, nil
		}
	}
	return "", fmt.Errorf("kmod not defined for vendor %s device %s", vendor, device)
}

func setupDpdkPorts(dpdkDriver string, pci pciArray, record map[string]*pciInfo) error {
	log.Printf("setupPorts: %+q\n", pci)
	for _, p := range pci {
		log.Printf("setupPorts: %s\n", p)
		info := &pciInfo{}
		record[p] = info
		info.wasKernelPort = false
		out, _ := ioutil.ReadFile(pciDeviceDir + p + "/vendor")
		info.vendor = strings.TrimSpace(string(out))
		out, _ = ioutil.ReadFile(pciDeviceDir + p + "/device")
		info.device = strings.TrimSpace(string(out))
		kmod, err := getDriverFromDeviceVendor(info.vendor, info.device)
		if err != nil {
			log.Fatal(err)
		}
		info.kmod = kmod
		bound, driver := isDeviceBound(p)
		if bound {
			info.driverPre = driver
			if isKernelDevice(p) {
				info.kmod = driver
				info.wasKernelPort = true
			} else if driver == dpdkDriver {
				// already on dpdk driver, skip
				info.driverCur = dpdkDriver
				continue
			}
			// unbind first
			unbind(p)
		}
		// set up new_id if not done yet
		if err := pciNewID(info.vendor, info.device, dpdkDriver); err != nil {
			return err
		}
		// small sleep to get new_id kick in
		time.Sleep(20 * time.Millisecond)
		if success, _ := isDeviceBound(p); !success {
			// bind the driver only if new_id didn't do the trick
			if err := bind(p, dpdkDriver); err != nil {
				return err
			}
		}
		info.driverCur = dpdkDriver
		pciRemoveID(info.vendor, info.device, dpdkDriver)
		time.Sleep(20 * time.Millisecond)
	}
	return nil
}

func restoreKernalPorts(pci pciArray, record map[string]*pciInfo) error {
	for _, p := range pci {
		if record[p].wasKernelPort {
			log.Printf("bind %s to %s\n", p, record[p].kmod)
			if err := unbind(p); err != nil {
				return err
			}
			time.Sleep(20 * time.Millisecond)
			if err := bind(p, record[p].kmod); err != nil {
				return err
			}
			time.Sleep(20 * time.Millisecond)
		}
	}
	return nil
}

func main() {
	autoStart := flag.Bool("auto", false, "auto start in io mode")
	queues := flag.Int("queues", 1, "number of rxq/txq")
	ring := flag.Int("ring-size", 2048, "ring size")
	var pci pciArray
	flag.Var(&pci, "pci", "pci address, can specify multiple times")
	testpmdPath := flag.String("testpmd-path", "testpmd", "if not in PATH, specify the testpmd location")
	flag.Parse()
	pciRecord := make(map[string]*pciInfo)
	if err := setupDpdkPorts("vfio-pci", pci, pciRecord); err != nil {
		log.Fatal(err)
	}
	t := testpmd{}
	if err := t.init(pci, *queues, *ring, *testpmdPath); err != nil {
		log.Fatalf("%v", err)
	}
	if *autoStart {
		if err := t.ioMode(); err != nil {
			log.Fatal(err)
		}
	}
	sigs := make(chan os.Signal, 1)
	signal.Notify(sigs, syscall.SIGINT, syscall.SIGTERM)
	for {
		select {
		case <-sigs:
			t.stop()
			if err := restoreKernalPorts(pci, pciRecord); err != nil {
				log.Fatal(err)
			}
			os.Exit(0)
		}
	}
}