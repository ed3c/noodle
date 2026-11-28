package admission

import (
	"errors"
	"path/filepath"
	"sort"
	"strings"
)

// ReadMachineHostReservations observes unresolved capacity at exactly one
// Host root. Other Host paths stay outside its scope.
func ReadMachineHostReservations(directory, root string) ([]string, error) {
	if !filepath.IsAbs(root) || filepath.Clean(root) != root || root == string(filepath.Separator) {
		return nil, errors.New("original machine Host root is invalid")
	}
	machine, err := readArchivedMachineJournal(directory)
	if err != nil {
		return nil, err
	}
	var ids []string
	for id, row := range machine.Leases {
		if !strings.HasPrefix(row.Owner, root+"/") && (row.Preparation == nil || row.Preparation.Source.Bindings["host_root"] != root) {
			continue
		}
		if err := errors.Join(validateMachinePreparation(row), validateMachineLeaseSettlement(row)); err != nil {
			return nil, err
		}
		if row.Settlement == nil && row.PreparationClosed == nil {
			ids = append(ids, id)
		}
	}
	sort.Strings(ids)
	return ids, nil
}
