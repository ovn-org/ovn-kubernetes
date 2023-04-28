/*----------------------------------------------------
 *
 *  2022 NVIDIA CORPORATION & AFFILIATES
 *
 *  Licensed under the Apache License, Version 2.0 (the License);
 *  you may not use this file except in compliance with the License.
 *  You may obtain a copy of the License at
 *
 *      http://www.apache.org/licenses/LICENSE-2.0
 *
 *  Unless required by applicable law or agreed to in writing, software
 *  distributed under the License is distributed on an AS IS BASIS,
 *  WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
 *  See the License for the specific language governing permissions and
 *  limitations under the License.
 *
 *----------------------------------------------------
 */

package sriovnet

import (
	"fmt"
	"strings"

	utilfs "github.com/k8snetworkplumbingwg/sriovnet/pkg/utils/filesystem"
)

func getFileNamesFromPath(dir string) ([]string, error) {
	_, err := utilfs.Fs.Stat(dir)
	if err != nil {
		return nil, fmt.Errorf("could not stat the directory %s: %v", dir, err)
	}

	files, err := utilfs.Fs.ReadDir(dir)
	if err != nil {
		return nil, fmt.Errorf("failed to read directory %s: %v", dir, err)
	}

	netDevices := make([]string, 0, len(files))
	for _, netDeviceFile := range files {
		netDevices = append(netDevices, strings.TrimSpace(netDeviceFile.Name()))
	}
	return netDevices, nil
}
