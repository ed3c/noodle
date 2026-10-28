package runtime

import "fmt"

func VerifyProgressMaintenanceHandoff(ref SessionReadbackFileRef) error {
	var p ProgressMaintenance
	if err := maintenanceVerify(ref, &p); err != nil {
		return err
	}
	var d openShellDispatchReceipt
	if _, err := ReadSessionReadbackFile(p.Sources["dispatch"].Path, &d); err != nil {
		return err
	}
	c, err := maintenanceCompletion(d)
	if err != nil {
		return err
	}
	if c.Plan != ref || c.ContextID != p.ContextID {
		return fmt.Errorf("maintenance handover plan differs")
	}
	var original OpenShellCustodyState
	if err := maintenanceVerify(c.OriginalCustody, &original); err != nil {
		return err
	}
	for _, record := range original.Records {
		if record.Worker.Identity.SessionID == c.SessionID {
			_, err := maintenanceStopped(ref, p, record)
			return err
		}
	}
	return fmt.Errorf("maintenance original attempt is missing")
}
