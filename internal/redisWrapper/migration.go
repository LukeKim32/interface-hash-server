package redisWrapper

import (
	"fmt"
	"interface_hash_server/internal/redisWrapper/templates"
	"interface_hash_server/tools"

	"github.com/gomodule/redigo/redis"
)

/************* Docker를 계속 restart 해준다는 가정 ******************/

// migrateDataToOtherNodes reads @deadNodes's data logs and records to other appropriate nodes
/* This function is for redistributing data of dead @deadNode
 */
func migrateDataToOtherNodes(deadNode RedisClient) error {

	// Read source node's data log file
	hashIndexToKeyValueMap := make(map[uint16]map[string]string)
	if err := getLatestDataFromLog(deadNode.Address, hashIndexToKeyValueMap); err != nil {
		return err
	}

	// Remove Dead (Master) node's log file
	if err := removeDataLogs(deadNode.Address); err != nil {
		return err
	}

	// Remove Dead (Slave) node's log file
	deadSlave, isExist := masterSlaveMap[deadNode.Address]
	if isExist {
		removeDataLogs(deadSlave.Address)
	}

	// Record source node's data into other appropriate nodes
	for hashIndex, keyValueMap := range hashIndexToKeyValueMap {
		// 이미 Hash Slot Map은 초기화 되어있음
		destinationNode := HashSlotMap[hashIndex]

		for eachKey, eachValue := range keyValueMap {

			fmt.Printf("%s에 원래 있던 (%s, %s) 를 다른 노드 %s 에..\n", deadNode.Address, eachKey, eachValue, destinationNode.Address)

			// Save the key-value data to destination Master node
			if _, err := redis.String(destinationNode.Connection.Do("SET", eachKey, eachValue)); err != nil {
				return err
			}

			// Destination Node가 중간에 죽어도, Log file에는 기록을 해놓는다
			// 데이터를 옮긴 마스터 노드의 데이터 로그에 기록
			if err := RecordModificationLog(destinationNode.Address, "SET", eachKey, eachValue); err != nil {
				return fmt.Errorf("redistruibuteHashSlot() : logging for recording dead node(%s)'s data failed", deadNode.Address)
			}

			// 데이터를 다른 마스터로 옮긴 후, 옮긴 마스터의 슬레이브에게도 전파
			ReplicateToSlave(destinationNode, "SET", eachKey, eachValue)
		}
	}

	return nil

}

// migrateDataToOtherNodes reads @deadNodes's data logs and records to other appropriate nodes
/* This function is for redistributing data of dead @deadNode
 * 이 함수 내부에서 targetNode가 죽을 경우 데이터는 손실될 수 있다.
 */
func reshardDataTo(targetNode RedisClient) error {

	for _, sourceMasterNode := range redisMasterClients {
		if sourceMasterNode.Address != targetNode.Address {

			// Read Other Master nodes' data log file
			hashIndexToKeyValueMap := make(map[uint16]map[string]string)

			if err := getLatestDataFromLog(sourceMasterNode.Address, hashIndexToKeyValueMap); err != nil {
				return err
			}

			// Recreate Data log file to Record latest data
			if err := removeDataLogs(sourceMasterNode.Address); err != nil {
				return err
			}
			if err := createDataLogFile(sourceMasterNode.Address); err != nil {
				return err
			}

			// Record source node's data into other appropriate nodes
			for hashIndex, keyValueMap := range hashIndexToKeyValueMap {
				destinationNode := HashSlotMap[hashIndex]

				// 현재 Hash map의 특정 Hash Index가 source Node 담당이라면, source Node 로그에 기록/갱신한다.
				if destinationNode.Address == sourceMasterNode.Address {

					for eachKey, eachValue := range keyValueMap {

						if err := RecordModificationLog(destinationNode.Address, "SET", eachKey, eachValue); err != nil {
							return fmt.Errorf("reshardDataTo() : logging for recording dead node(%s)'s data failed", destinationNode.Address)
						}
					}

				} else {
					// 현재 Hash map의 특정 Hash Index가 targetNode에 재할당 된 것이라면, targetNode에 저장한다.
					// (sourceNode에서는 Delete 연산 수행)

					for eachKey, eachValue := range keyValueMap {

						// Delete the key-value data from source Master node
						if _, err := redis.String(sourceMasterNode.Connection.Do("DEL", eachKey)); err != nil {
							return fmt.Errorf("reshardDataTo() : deleting from source node error - %s", err.Error())
						}

						fmt.Printf("%s에 원래 있던 (%s, %s) 를 다른 노드 %s 에..\n", sourceMasterNode.Address, eachKey, eachValue, destinationNode.Address)

						// Save the key-value data to destination Master node
						if _, err := redis.String(destinationNode.Connection.Do("SET", eachKey, eachValue)); err != nil {
							return err
						}

						// Destination Node가 중간에 죽어도, Log file에는 기록을 해놓는다
						// 데이터를 옮긴 마스터 노드의 데이터 로그에 기록
						if err := RecordModificationLog(destinationNode.Address, "SET", eachKey, eachValue); err != nil {
							return fmt.Errorf("reshardDataTo() : logging for resharding to new node(%s)'s data failed", destinationNode.Address)
						}

						// 데이터를 다른 마스터로 옮긴 후, 옮긴 마스터의 슬레이브에게도 전파
						ReplicateToSlave(destinationNode, "SET", eachKey, eachValue)
					}
				}
			}
		}
	}

	return nil

}

func checkAliveAndSaveData(destinationNode RedisClient, key string, value string) error {

	if _, err := redis.String(destinationNode.Connection.Do("SET", key, value)); err != nil {

		fmt.Printf("(%s, %s) 를 다른 노드 %s 에 재분배 중 실패\n", key, value, destinationNode.Address)

		// 명령이 실패한 경우 - redistruibuteHashSlot() Hash Slot 할당 과정에서 죽었을 수도 있으므로
		numberOfTotalVotes := len(monitorNodeAddressList) + 1
		votes, err := askRedisIsAliveToMonitors(destinationNode)
		if err != nil {
			return fmt.Errorf("checkAliveAndSaveData() : ask Redis Node is alive failed - %s", err.Error())
		}

		tools.InfoLogger.Printf(templates.FailOverVoteResult, destinationNode.Address, votes, numberOfTotalVotes)

		if votes > (numberOfTotalVotes / 2) {
			// Retry the command ignoring if error exists
			destinationNode.Connection.Do("SET", key, value)

		} else { // 죽었다고 판단할 경우, 명령어 수행은 그만하되 Log file에는 기록을 해놓는다
			return nil
		}
	}

	return nil

}
