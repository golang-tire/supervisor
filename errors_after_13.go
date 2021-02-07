// +build go1.13

package supervisor

import "errors"

func isErr(err error, target error) bool {
	return errors.Is(err, target)
}
