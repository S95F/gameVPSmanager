// This fixture is the CONTROL for the two "must not compile" programs beside
// it. It does the same work the right way round, so that if it ever stops
// building, the compile-fail assertions are known to be worthless rather than
// quietly passing for the wrong reason.
package main

import (
	"fmt"

	"github.com/S95F/gameVPSmanager/internal/content"
)

func main() {
	acquire := []content.AcquireID{"3171167894", "3508537032"}
	activate := []content.ActivationID{"ThatDamnLibrary", "NeatUIFramework"}

	acq := content.AcquisitionSpec{Key: "WorkshopItems", Separator: ";"}
	act := content.ActivationSpec{Key: "Mods", Separator: ";"}

	fmt.Println(acq.Line(acquire).Value())
	fmt.Println(act.Line(activate).Value())
}
