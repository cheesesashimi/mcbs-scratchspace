package main

import (
	"flag"
	"io/ioutil"
	"os"
	"path/filepath"

	"k8s.io/klog/v2"

	ign3types "github.com/coreos/ignition/v2/config/v3_2/types"
	mcfgv1 "github.com/openshift/machine-config-operator/pkg/apis/machineconfiguration.openshift.io/v1"
	ctrlcommon "github.com/openshift/machine-config-operator/pkg/controller/common"

	"github.com/ghodss/yaml"
	nfpm "github.com/goreleaser/nfpm/v2"
	nfpmFiles "github.com/goreleaser/nfpm/v2/files"
	_ "github.com/goreleaser/nfpm/v2/rpm" // blank import to register the format
	"github.com/vincent-petithory/dataurl"
)

type wrappedMachineConfig struct {
	mcfgv1.MachineConfig
	ign3 *ign3types.Config
}

func (w *wrappedMachineConfig) GetNFPMConfig(dirName string) (nfpm.Config, error) {
	klog.Infof("Building RPM config")

	conf := nfpm.Config{
		Info: nfpm.Info{
			Name:     w.ObjectMeta.Name,
			Arch:     "amd64",
			Platform: "linux",
			Version:  "v1.0.0",
			Section:  "default",
			Overridables: nfpm.Overridables{
				Provides:   []string{"bar"},
				Depends:    []string{"foo", "bar"},
				Replaces:   []string{"foobar"},
				Recommends: []string{"whatever"},
				Suggests:   []string{"something-else"},
				Conflicts:  []string{"not-foo", "not-bar"},
				RPM:        nfpm.RPM{},
			},
		},
	}

	ign3, err := w.GetIgnitionConfig()
	if err != nil {
		return conf, err
	}

	contents := nfpmFiles.Contents{}

	for _, ignFile := range ign3.Storage.Files {
		contents = append(contents, &nfpmFiles.Content{
			Source:      filepath.Join(dirName, ignFile.Path),
			Destination: ignFile.Path,
		})
	}

	conf.Info.Overridables.Contents = contents

	confErr := conf.Validate()

	return conf, confErr
}

func (w *wrappedMachineConfig) GetIgnitionConfig() (ign3types.Config, error) {
	if w.ign3 != nil {
		return *w.ign3, nil
	}

	klog.Infof("Parsing ignition config from machine config")

	ign3, err := ctrlcommon.ParseAndConvertConfig(w.Spec.Config.Raw)
	if err != nil {
		return ign3types.Config{}, err
	}

	w.ign3 = &ign3
	return ign3, nil
}

func (w *wrappedMachineConfig) WriteIgnition(dirName string) error {
	ign3, err := w.GetIgnitionConfig()
	if err != nil {
		return err
	}

	klog.Infof("Writing ignition files to disk under %s", dirName)

	for _, file := range ign3.Storage.Files {
		if err := writeIgnFileToDisk(dirName, file); err != nil {
			return err
		}
	}

	return nil
}

func readMachineConfig(filename string) (wrappedMachineConfig, error) {
	wmc := wrappedMachineConfig{}

	klog.Infof("Reading machine config from: %s", filename)

	inBytes, err := ioutil.ReadFile(filename)
	if err != nil {
		return wmc, err
	}

	// Note: This will only retrieve a single MachineConfig from within a multi-document YAML file.
	if err := yaml.Unmarshal(inBytes, &wmc.MachineConfig); err != nil {
		return wmc, err
	}

	if err := ctrlcommon.ValidateMachineConfig(wmc.Spec); err != nil {
		return wmc, err
	}

	klog.Infof("Machine config %s validated", wmc.GetName())

	return wmc, nil
}

func writeIgnFileToDisk(dirName string, file ign3types.File) error {
	fullPath := filepath.Join(dirName, file.Path)

	if err := os.MkdirAll(filepath.Dir(fullPath), 0755); err != nil {
		return err
	}

	contents := &dataurl.DataURL{}
	if file.Contents.Source != nil {
		var err error
		contents, err = dataurl.DecodeString(*file.Contents.Source)
		if err != nil {
			return err
		}
	}

	klog.Infof("Writing %s", file.Path)

	return ioutil.WriteFile(fullPath, contents.Data, 0755)
}

func toRPM(conf nfpm.Config) error {
	info, err := conf.Get("rpm")
	if err != nil {
		return err
	}

	if err := nfpm.Validate(info); err != nil {
		return err
	}

	pkger, err := nfpm.Get("rpm")
	if err != nil {
		return err
	}

	target := pkger.ConventionalFileName(info)

	pkgFile, err := os.Create(target)
	if err != nil {
		return err
	}

	defer pkgFile.Close()

	info.Target = target

	klog.Infof("Writing RPM to %s", target)

	if err := pkger.Package(info, pkgFile); err != nil {
		os.Remove(target)
		return err
	}

	return nil
}

func main() {

	klogFlags := flag.NewFlagSet("mcfg2rpm", flag.ExitOnError)
	klog.InitFlags(klogFlags)
	klog.SetOutput(os.Stderr)

	// Sync the glog and klog flags; MCO uses glog
	flag.CommandLine.VisitAll(func(f1 *flag.Flag) {
		f2 := klogFlags.Lookup(f1.Name)
		if f2 != nil {
			value := f1.Value.String()
			f2.Value.Set(value)
		}
	})

	if len(os.Args) == 1 {
		klog.Exit("no input file provided")
	}

	wmc, err := readMachineConfig(os.Args[1])
	if err != nil {
		klog.Exit(err)
	}

	tempDir, err := ioutil.TempDir("", "mcfg2rpm")
	if err != nil {
		klog.Exit(err)
	}

	defer func() {
		klog.Infof("Removing temp dir %s", tempDir)
		os.RemoveAll(tempDir)
	}()

	if err := wmc.WriteIgnition(tempDir); err != nil {
		klog.Exit(err)
	}

	nfpmConfig, err := wmc.GetNFPMConfig(tempDir)
	if err != nil {
		klog.Exit(err)
	}

	if err := toRPM(nfpmConfig); err != nil {
		klog.Exit(err)
	}
}
