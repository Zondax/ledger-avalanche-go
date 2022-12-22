/*******************************************************************************
*   (c) 2018 - 2022 ZondaX AG
*
*  Licensed under the Apache License, Version 2.0 (the "License");
*  you may not use this file except in compliance with the License.
*  You may obtain a copy of the License at
*
*      http://www.apache.org/licenses/LICENSE-2.0
*
*  Unless required by applicable law or agreed to in writing, software
*  distributed under the License is distributed on an "AS IS" BASIS,
*  WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
*  See the License for the specific language governing permissions and
*  limitations under the License.
// ********************************************************************************/

package ledger_avalanche_go

import (
	"errors"
	"github.com/zondax/ledger-go"
)

// FindLedgerAvalancheApp FindLedgerAvalancheUserApp finds a Avax user app running in a ledger device
func FindLedgerAvalancheApp() (_ *LedgerAvalanche, rerr error) {
	ledgerAdmin := ledger_go.NewLedgerAdmin()
	ledgerAPI, err := ledgerAdmin.Connect(0)
	if err != nil {
		return nil, err
	}

	defer func() {
		if rerr != nil {
			ledgerAPI.Close()
		}
	}()

	app := &LedgerAvalanche{ledgerAPI, VersionInfo{}}
	appVersion, err := app.GetVersion()
	if err != nil {
		if err.Error() == "[APDU_CODE_CLA_NOT_SUPPORTED] CLA not supported" {
			err = errors.New("are you sure the Avalanche app is open?")
		}
		return nil, err
	}

	if err := app.CheckVersion(*appVersion); err != nil {
		return nil, err
	}

	return app, err
}

// Close closes a connection with the Avalanche user app
func (ledger *LedgerAvalanche) Close() error {
	return ledger.api.Close()
}

// CheckVersion returns true if the App version is supported by this library
func (ledger *LedgerAvalanche) CheckVersion(ver VersionInfo) error {
	version, err := ledger.GetVersion()
	if err != nil {
		return err
	}

	return CheckVersion(*version, VersionInfo{0, 0, 6, 5})
}

// GetVersion returns the current version of the Cosmos user app
func (ledger *LedgerAvalanche) GetVersion() (*VersionInfo, error) {
	message := []byte{CLA, INS_GET_VERSION, 0, 0, 0}
	response, err := ledger.api.Exchange(message)

	if err != nil {
		return nil, err
	}

	if len(response) < 4 {
		return nil, errors.New("invalid response")
	}

	ledger.version = VersionInfo{
		AppMode: response[0],
		Major:   response[1],
		Minor:   response[2],
		Patch:   response[3],
	}

	return &ledger.version, nil
}

// GetPubKey returns the pubkey and hash
func (ledger *LedgerAvalanche) GetPubKey(path string, show bool, hrp string, chainid string) (publicKey []byte, hash []byte, err error) {
	if len(hrp) > 83 {
		return nil, nil, errors.New("hrp len should be < 83 chars")
	}

	serializedHRP, err := SerializeHrp(hrp)
	if err != nil {
		return nil, nil, err
	}

	serializedPath, err := SerializePath(path)
	if err != nil {
		return nil, nil, err
	}

	serializedChainID, err := SerializeChainID(chainid)
	if err != nil {
		return nil, nil, err
	}

	p1 := byte(P1_ONLY_RETRIEVE)
	if show {
		p1 = byte(P1_SHOW_ADDRESS_IN_DEVICE)
	}

	// Prepare message
	header := []byte{CLA, INS_GET_ADDR, p1, 0, 0}
	message := append(header, serializedHRP...)
	message = append(message, serializedChainID...)
	message = append(message, serializedPath...)
	message[4] = byte(len(message) - len(header)) // update length

	response, err := ledger.api.Exchange(message)

	if err != nil {
		return nil, nil, err
	}

	if len(response) < 35+len(hrp) {
		return nil, nil, errors.New("Invalid response")
	}

	// [publicKeyLen | publicKey | hash | errorCode]
	publicKeyLen := response[0]
	publicKey = response[1 : publicKeyLen+1]
	hash = response[publicKeyLen+1 : len(response)]

	return publicKey, hash, err
}

func (ledger *LedgerAvalanche) Sign(pathPrefix string, signingPaths []string, message []byte, changePaths []string) (*ResponseSign, error) {
	paths := signingPaths
	if changePaths != nil {
		paths = append(paths, changePaths...)
		paths = RemoveDuplicates(paths)
	}

	serializedPath, err := SerializePath(pathPrefix)
	if err != nil {
		return nil, err
	}

	payloadType := PAYLOAD_INIT
	p2 := FIRST_MESSAGE
	header := []byte{CLA, INS_SIGN, byte(payloadType), byte(p2), byte(len(serializedPath))}
	bytesToSend := append(header, serializedPath...)
	_, err = ledger.api.Exchange(bytesToSend)
	if err != nil {
		return nil, errors.New("command rejected")
	}

	msg := ConcatMessageAndChangePath(message, paths)

	for i := 0; i < len(msg); i += CHUNK_SIZE {
		end := i + CHUNK_SIZE
		payloadType := PAYLOAD_ADD
		p2 := 0

		if end > len(msg) {
			end = len(msg)
			payloadType = PAYLOAD_LAST
		}
		chunk := msg[i:end]
		chunkSize := end - i

		header := []byte{CLA, INS_SIGN, byte(payloadType), byte(p2), byte(chunkSize)}
		bytesToSend := append(header, chunk...)
		response, err := ledger.api.Exchange(bytesToSend)
		if err != nil {
			if err.Error() == "[APDU_CODE_BAD_KEY_HANDLE] The parameters in the data field are incorrect" {
				// In this special case, we can extract additional info
				errorMsg := string(response)
				return nil, errors.New(errorMsg)
			}
			if err.Error() == "[APDU_CODE_DATA_INVALID] Referenced data reversibly blocked (invalidated)" {
				errorMsg := string(response)
				return nil, errors.New(errorMsg)
			}
			return nil, err
		}
	}

	// Transaction was approved so start iterating over signing_paths to sign
	// and collect each signature
	return SignAndCollect(signingPaths, ledger)
}

func SignAndCollect(signingPaths []string, ledger *LedgerAvalanche) (*ResponseSign, error) {
	// Where each pair path_suffix, signature are stored
	signatures := make(map[string][]byte)

	for idx, suffix := range signingPaths {
		pathBuf, err := SerializePathSuffix(suffix)
		if err != nil {
			return nil, err
		}

		p1 := LAST_MESSAGE
		if idx < len(signingPaths)-1 {
			p1 = NEXT_MESSAGE
		}

		// Send path to sign hash that should be in device's ram memory
		header := []byte{CLA, INS_SIGN_HASH, byte(p1), byte(0x00)}
		bytesToSend := append(header, pathBuf...)
		response, err := ledger.api.Exchange(bytesToSend)

		if err != nil {
			return nil, err
		}

		errorCodeData := response[len(response)-2:]
		returnCode := LedgerError(int(errorCodeData[0])*256 + int(errorCodeData[1]))

		if returnCode == NoErrors && len(response) > 2 {
			signatures[suffix] = response[:len(response)-2]
		} else if returnCode != NoErrors {
			return nil, errors.New("signing hash failed")
		}
	}

	return &ResponseSign{nil, signatures}, nil
}

func RemoveDuplicates(elements []string) []string {
	// Use map to record duplicates as we find them.
	encountered := map[string]bool{}
	result := []string{}

	for v := range elements {
		if encountered[elements[v]] == true {
			// Do not add duplicate.
		} else {
			// Record this element as an encountered element.
			encountered[elements[v]] = true
			result = append(result, elements[v])
		}
	}
	return result
}

func ConcatMessageAndChangePath(message []byte, path []string) []byte {
	msg := append([]byte{}, message...)
	if path == nil {
		return append([]byte{0}, msg...)
	}
	buffer := []byte{byte(len(path))}
	for _, element := range path {
		pathBuf, err := SerializePathSuffix(element)
		if err != nil {
			return nil
		}
		buffer = append(buffer, pathBuf...)
	}
	return append(buffer, msg...)
}
