// +build !go1.13

package supervisor

func isErr(err error, target error) bool {
	return err == target
}
