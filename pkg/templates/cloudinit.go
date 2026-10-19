package templates

import (
	"context"
	"fmt"
	"maps"
	"os"
	"path/filepath"
	"slices"

	"github.com/diskfs/go-diskfs/backend/file"
	"github.com/diskfs/go-diskfs/filesystem/iso9660"
	"github.com/luthermonson/go-proxmox"
)

const (
	// seedLabel is the volume label cloud-init's NoCloud source looks for.
	seedLabel    = "cidata"
	isoBlockSize = 2048
	privateDir   = 0o700
	privateFile  = 0o600
)

// attachSeed uploads a NoCloud seed holding files (user-data, meta-data, network-config) to the
// storage and attaches it to vm as device, a CD-ROM. go-proxmox's VirtualMachine.CloudInit does the
// same but gives the upload 5 seconds and the change 2, which a busy node overruns; here both wait
// as long as the builder's other tasks.
func attachSeed(ctx context.Context, node *proxmox.Node, storageName string, vm *proxmox.VirtualMachine, device string, files map[string]string) error {
	storage, err := node.Storage(ctx, storageName)
	if err != nil {
		return fmt.Errorf("reading storage %s: %w", storageName, err)
	}
	dir, err := os.MkdirTemp("", "kubehenge-seed-")
	if err != nil {
		return fmt.Errorf("making the cloud-init ISO: %w", err)
	}
	defer func() { _ = os.RemoveAll(dir) }()
	name := fmt.Sprintf(proxmox.UserDataISOFormat, vm.VMID)
	iso := filepath.Join(dir, name)
	if err = writeSeed(iso, files); err != nil {
		return err
	}
	// No checksum: Proxmox VE's proxy takes an upload's form fields only in the order content,
	// checksum-algorithm, checksum, and go-proxmox writes them in a map's order, so an upload with
	// a checksum is refused at random.
	task, err := storage.UploadWithName("iso", iso, name)
	if err = waitTask(ctx, task, err, shortTaskTimeout); err != nil {
		return fmt.Errorf("uploading the cloud-init ISO: %w", err)
	}
	task, err = vm.Config(ctx, proxmox.VirtualMachineOption{Name: device, Value: storage.Name + ":iso/" + name + ",media=cdrom"})
	if err = waitTask(ctx, task, err, configTimeout); err != nil {
		return fmt.Errorf("attaching the cloud-init ISO: %w", err)
	}
	return nil
}

// writeSeed writes files into a new ISO image at path, labelled for cloud-init. The image is built
// in a working directory beside it, which must not hold the image itself.
func writeSeed(path string, files map[string]string) (err error) {
	failed := func(err error) error { return fmt.Errorf("making the cloud-init ISO: %w", err) }
	work := filepath.Join(filepath.Dir(path), "work")
	if err = os.Mkdir(work, privateDir); err != nil {
		return failed(err)
	}
	f, err := os.OpenFile(path, os.O_CREATE|os.O_EXCL|os.O_WRONLY, privateFile) //nolint:gosec // see above
	if err == nil {
		err = f.Close()
	}
	if err != nil {
		return failed(err)
	}
	disk, err := file.OpenFromPath(path, false)
	if err != nil {
		return failed(err)
	}
	defer func() {
		if cerr := disk.Close(); err == nil && cerr != nil {
			err = failed(cerr)
		}
	}()
	fs, err := iso9660.Create(disk, 0, 0, isoBlockSize, work)
	if err != nil {
		return failed(err)
	}
	for _, name := range slices.Sorted(maps.Keys(files)) {
		if err = addFile(fs, name, files[name]); err != nil {
			return failed(err)
		}
	}
	if err = fs.Finalize(iso9660.FinalizeOptions{RockRidge: true, Joliet: true, VolumeIdentifier: seedLabel}); err != nil { //nolint:misspell // go-diskfs's name
		return failed(err)
	}
	return nil
}

// addFile writes a file at the image's root. Its size is recorded when it is closed, which has to
// come before the image is written.
func addFile(fs *iso9660.FileSystem, name, content string) error {
	w, err := fs.OpenFile("/"+name, os.O_CREATE|os.O_RDWR)
	if err != nil {
		return fmt.Errorf("adding %s: %w", name, err)
	}
	_, err = w.Write([]byte(content))
	if cerr := w.Close(); err == nil {
		err = cerr
	}
	if err != nil {
		return fmt.Errorf("adding %s: %w", name, err)
	}
	return nil
}
