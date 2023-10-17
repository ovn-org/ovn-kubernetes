package batching

import "fmt"

// Batch splits values in `data` in batches of size `batchSize`, and calls `eachFn` for each batch.
func Batch[T any](batchSize int, data []T, eachFn func([]T) error) error {
	if batchSize < 1 {
		return fmt.Errorf("batchSize should be > 0, got %d", batchSize)
	}
	start := 0
	dataLen := len(data)
	for start < dataLen {
		end := start + batchSize
		if end > dataLen {
			end = dataLen
		}
		err := eachFn(data[start:end])
		if err != nil {
			return err
		}
		start = end
	}
	return nil
}

// BatchMap splits values in `data` in batches of size `batchSize`, and calls `eachFn` for each batch.
// Values of a specific key may be split into different batches, make sure `eachFn` can handle that case.
func BatchMap[T any](batchSize int, data map[string][]T, eachFn func(map[string][]T) error) error {
	if batchSize < 1 {
		return fmt.Errorf("batchSize should be > 0, got %d", batchSize)
	}
	batchCounter := 0
	batchMap := map[string][]T{}

	for key, value := range data {
		// a given value may need to be split into multiple batches.
		// allElemsBatched is true when all value elements are added to the batchMap.
		allElemsBatched := false
		for !allElemsBatched {
			if batchCounter+len(value) >= batchSize {
				leftSpace := batchSize - batchCounter
				batchMap[key] = value[:leftSpace]

				err := eachFn(batchMap)
				if err != nil {
					return err
				}
				// reset batch
				batchMap = map[string][]T{}
				batchCounter = 0

				// update value to what's left
				value = value[leftSpace:]
				allElemsBatched = len(value) == 0
			} else {
				batchMap[key] = value
				batchCounter += len(value)
				allElemsBatched = true
			}
		}
	}
	if len(batchMap) > 0 {
		// process last batch
		err := eachFn(batchMap)
		if err != nil {
			return err
		}
	}
	return nil
}
