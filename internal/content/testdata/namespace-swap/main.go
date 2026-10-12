// This fixture MUST NOT COMPILE.
//
// It passes activation ids (mod.info `id=` values) to the acquisition write
// key, which is the single most expensive mistake a mod system can make: the
// server is told to download "ThatDamnLibrary", downloads nothing, and
// enables nothing -- with no error anywhere, because WorkshopItems= accepts
// any string. The compiler refusing this is the two-list model.
package main

import (
	"fmt"

	"github.com/S95F/gameVPSmanager/internal/content"
)

func main() {
	activate := []content.ActivationID{"ThatDamnLibrary", "NeatUIFramework"}
	acq := content.AcquisitionSpec{Key: "WorkshopItems", Separator: ";"}
	fmt.Println(acq.Line(activate).Value())
}
