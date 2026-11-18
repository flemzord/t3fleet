package fleet

import "strings"

const machineNamePrefix = "t3fleet-"

func managedName(name string) string {
	if strings.HasPrefix(name, machineNamePrefix) {
		return name
	}
	return machineNamePrefix + name
}
