package driver

import "os"

type OsClient interface {
	MkDirAllWithPerms(path string, perms os.FileMode, uid, gid int64) error
	Remove(path string) error
	RemoveAll(path string) error
}

type FakeOsClient struct{}

func (o *FakeOsClient) MkDirAllWithPerms(_ string, _ os.FileMode, _, _ int64) error {
	return nil
}

func (o *FakeOsClient) Remove(_ string) error {
	return nil
}

func (o *FakeOsClient) RemoveAll(_ string) error {
	return nil
}

type BrokenOsClient struct{}

func (o *BrokenOsClient) MkDirAllWithPerms(_ string, _ os.FileMode, _, _ int64) error {
	return &os.PathError{}
}

func (o *BrokenOsClient) Remove(_ string) error {
	return &os.PathError{}
}

func (o *BrokenOsClient) RemoveAll(_ string) error {
	return &os.PathError{}
}

type RealOsClient struct{}

func (o *RealOsClient) MkDirAllWithPerms(path string, perms os.FileMode, uid, gid int64) error {
	err := os.MkdirAll(path, perms)
	if err != nil {
		return err
	}
	err = os.Chown(path, int(uid), int(gid))
	if err != nil {
		return err
	}
	return nil
}

func (o *RealOsClient) Remove(path string) error {
	return os.Remove(path)
}

func (o *RealOsClient) RemoveAll(path string) error {
	return os.RemoveAll(path)
}
