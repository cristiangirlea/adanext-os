package source

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"golang.org/x/sys/windows"
)

func setRootTestDACL(t *testing.T, path, aces string) {
	t.Helper()
	user, err := windows.GetCurrentProcessToken().GetTokenUser()
	if err != nil {
		t.Fatal(err)
	}
	sd, err := windows.SecurityDescriptorFromString("D:P(A;OICI;FA;;;" + user.User.Sid.String() + ")" + aces)
	if err != nil {
		t.Fatal(err)
	}
	acl, _, err := sd.DACL()
	if err != nil {
		t.Fatal(err)
	}
	if err := windows.SetNamedSecurityInfo(path, windows.SE_FILE_OBJECT, windows.DACL_SECURITY_INFORMATION|windows.PROTECTED_DACL_SECURITY_INFORMATION, nil, nil, acl, nil); err != nil {
		t.Fatal(err)
	}
}

func TestImportRootRefusesEveryoneWrite(t *testing.T) {
	root := permissionTestChild(t, permissionTestTop(t), "root")
	child := permissionTestChild(t, root, "child")
	setRootTestDACL(t, child, "(A;;GW;;;WD)")
	assertRootCode(t, func() error { _, err := CanonicalImportRoot(child); return err }(), "root_writable_by_others")
	assertRootCode(t, func() error { _, _, err := BrowseImportRoot(root, "child"); return err }(), "root_writable_by_others")
	setRootTestDACL(t, root, "(A;;GW;;;WD)")
	assertRootCode(t, func() error { _, _, err := BrowseImportRoot(root, ""); return err }(), "root_writable_by_others")
}

func TestImportRootRefusesInheritOnlyWrite(t *testing.T) {
	root := permissionTestChild(t, permissionTestTop(t), "root")
	setRootTestDACL(t, root, "(A;OICIIO;GW;;;WD)")
	_, err := CanonicalImportRoot(root)
	assertRootCode(t, err, BlockRootWritable)
	_, _, err = BrowseImportRoot(root, "")
	assertRootCode(t, err, BlockRootWritable)
}

func TestImportRootRefusesNonLocalVolumeBeforeOpen(t *testing.T) {
	root := filepath.Join(tempDir(t), "missing")
	original := importDriveType
	t.Cleanup(func() { importDriveType = original })
	for _, kind := range []uint32{windows.DRIVE_REMOTE, windows.DRIVE_NO_ROOT_DIR} {
		importDriveType = func(volume *uint16) uint32 {
			if got := windows.UTF16PtrToString(volume); got != volumeRoot(root) {
				t.Errorf("volume: %s", got)
			}
			return kind
		}
		assertRootCode(t, func() error { _, err := CanonicalImportRoot(root); return err }(), BlockRemotePath)
		assertRootCode(t, func() error { _, _, err := BrowseImportRoot(root, ""); return err }(), BlockRemotePath)
	}
}

func TestImportRootShortNameUsesDirectoryIdentity(t *testing.T) {
	trustImportRoots(t)
	root := tempDir(t)
	check := func(root string) bool {
		t.Helper()
		child := filepath.Join(root, "Long folder name")
		if err := os.Mkdir(child, 0700); err != nil {
			t.Fatal(err)
		}
		p, err := windows.UTF16PtrFromString(child)
		if err != nil {
			t.Fatal(err)
		}
		buf := make([]uint16, 32768)
		n, err := windows.GetShortPathName(p, &buf[0], uint32(len(buf)))
		if err != nil || n >= uint32(len(buf)) {
			t.Skipf("cannot query short names: %v", err)
		}
		alias := filepath.Base(windows.UTF16ToString(buf[:n]))
		if strings.EqualFold(alias, filepath.Base(child)) {
			return false
		}
		if _, _, err := BrowseImportRoot(root, alias); err != nil {
			t.Fatal(err)
		}
		short, err := OpenImportDirectory(root, alias)
		if err != nil {
			t.Fatal(err)
		}
		defer short.Close()
		long, err := OpenImportDirectory(root, strings.ToUpper(filepath.Base(child)))
		if err != nil {
			t.Fatal(err)
		}
		defer long.Close()
		if !SameDirectory(short, long) {
			t.Fatal("alias did not identify the same directory")
		}
		git(t, child, "init", "-q")
		got, err := newInspector(t).InspectInRoot(t.Context(), short.Name(), root)
		if err != nil || len(got.Blockers) != 0 {
			t.Fatalf("inspect through short name: %v %v", got.Blockers, err)
		}
		inspected, err := OpenSourceDirectory(got.Identity.Path)
		if err != nil {
			t.Fatal(err)
		}
		defer inspected.Close()
		if !SameDirectory(short, inspected) {
			t.Fatal("git inspected a different directory")
		}
		return true
	}
	if check(root) {
		return
	}
	if local := os.Getenv("LOCALAPPDATA"); filepath.IsAbs(local) && !strings.EqualFold(filepath.VolumeName(local), filepath.VolumeName(root)) {
		other, err := os.MkdirTemp(filepath.Join(local, "Temp"), "ada-short-name-")
		if err == nil {
			t.Cleanup(func() { os.RemoveAll(other) })
			if check(other) {
				return
			}
		} else {
			t.Logf("alternate temp volume unavailable: %v", err)
		}
	}
	t.Skip("no 8.3 alias on available temporary volumes")
}

func TestImportRootChecksWindowsOwner(t *testing.T) {
	root := permissionTestChild(t, permissionTestTop(t), "root")
	fakeImportSecurity(t, root, "O:WDD:(A;;FA;;;SY)")
	_, err := CanonicalImportRoot(root)
	assertRootCode(t, err, "root_writable_by_others")
}

func TestImportRootChecksWindowsAncestorDeleteChild(t *testing.T) {
	parent := permissionTestChild(t, permissionTestTop(t), "parent")
	root := permissionTestChild(t, parent, "root")
	setRootTestDACL(t, parent, "(A;;0x6;;;WD)")
	if _, err := CanonicalImportRoot(root); err != nil {
		t.Fatal(err)
	}
	setRootTestDACL(t, parent, "(A;;0x40;;;WD)")
	_, err := CanonicalImportRoot(root)
	assertRootCode(t, err, "root_writable_by_others")
	_, _, err = BrowseImportRoot(root, "")
	assertRootCode(t, err, "root_writable_by_others")
	setRootTestDACL(t, parent, "")
	setRootTestDACL(t, root, "(A;;SD;;;WD)")
	_, err = CanonicalImportRoot(root)
	assertRootCode(t, err, "root_writable_by_others")
}

func TestImportRootChecksWindowsIntermediateAncestorRights(t *testing.T) {
	parent := permissionTestChild(t, permissionTestTop(t), "parent")
	root := permissionTestChild(t, parent, "root")
	for _, rights := range []string{"SD", "WD", "WO"} {
		t.Run(rights, func(t *testing.T) {
			setRootTestDACL(t, parent, "(A;;"+rights+";;;WD)")
			_, err := CanonicalImportRoot(root)
			assertRootCode(t, err, BlockRootWritable)
			_, _, err = BrowseImportRoot(root, "")
			assertRootCode(t, err, BlockRootWritable)
		})
	}
}

func fakeImportSecurity(t *testing.T, path, descriptor string) {
	t.Helper()
	original := importSecurityInfo
	t.Cleanup(func() { importSecurityInfo = original })
	importSecurityInfo = func(handle windows.Handle, kind windows.SE_OBJECT_TYPE, info windows.SECURITY_INFORMATION) (*windows.SECURITY_DESCRIPTOR, error) {
		var buf [32768]uint16
		n, err := windows.GetFinalPathNameByHandle(handle, &buf[0], uint32(len(buf)), 0)
		if err != nil {
			return nil, err
		}
		if strings.EqualFold(strings.TrimPrefix(windows.UTF16ToString(buf[:n]), `\\?\`), path) {
			return windows.SecurityDescriptorFromString(descriptor)
		}
		return original(handle, kind, info)
	}
}

func TestImportRootChecksWindowsIntermediateAncestorOwner(t *testing.T) {
	parent := permissionTestChild(t, permissionTestTop(t), "parent")
	root := permissionTestChild(t, parent, "root")
	t.Run("foreign", func(t *testing.T) {
		fakeImportSecurity(t, parent, "O:WDD:(A;;FA;;;SY)")
		_, err := CanonicalImportRoot(root)
		assertRootCode(t, err, BlockRootWritable)
	})
	t.Run("TrustedInstaller", func(t *testing.T) {
		fakeImportSecurity(t, parent, "O:S-1-5-80-956008885-3418522649-1831038044-1853292631-2271478464D:(A;;FA;;;SY)")
		if _, err := CanonicalImportRoot(root); err != nil {
			t.Fatal(err)
		}
	})
}

func TestImportRootChecksWindowsVolumeRoot(t *testing.T) {
	top := permissionTestTop(t)
	root := permissionTestChild(t, top, "root")
	setRootTestDACL(t, root, "")
	t.Run("create-only", func(t *testing.T) {
		setRootTestDACL(t, top, "(A;;0x6;;;AU)(A;OICIIO;SDGXGWGR;;;AU)")
		if _, err := CanonicalImportRoot(root); err != nil {
			t.Fatal(err)
		}
	})
	t.Run("delete-child", func(t *testing.T) {
		setRootTestDACL(t, top, "(A;;0x40;;;AU)")
		_, err := CanonicalImportRoot(root)
		assertRootCode(t, err, BlockRootWritable)
	})
	for _, rights := range []string{"WD", "WO"} {
		t.Run(rights, func(t *testing.T) {
			setRootTestDACL(t, top, "(A;;"+rights+";;;AU)")
			_, err := CanonicalImportRoot(root)
			assertRootCode(t, err, BlockRootWritable)
		})
	}
	t.Run("foreign-owner", func(t *testing.T) {
		fakeImportSecurity(t, top, "O:WDD:(A;;FA;;;SY)")
		_, err := CanonicalImportRoot(root)
		assertRootCode(t, err, BlockRootWritable)
	})
	t.Run("TrustedInstaller", func(t *testing.T) {
		fakeImportSecurity(t, top, "O:S-1-5-80-956008885-3418522649-1831038044-1853292631-2271478464D:(A;;FA;;;SY)")
		if _, err := CanonicalImportRoot(root); err != nil {
			t.Fatal(err)
		}
	})
}

func TestChooserAllowsWritableMetadataWithinRoot(t *testing.T) {
	testChooserChecksAlternateBoundaries(t, func(path string) {
		setRootTestDACL(t, path, "(A;;GW;;;WD)")
	})
}
