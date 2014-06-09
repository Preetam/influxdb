package datastore

import (
	"cluster"

	"parser"
	"protocol"

	"bytes"
	"encoding/binary"
	"errors"
	"os"
	"sync"

	"code.google.com/p/goprotobuf/proto"
	"github.com/VividCortex/bolt"
)

type BoltShard struct {
	baseDir string
	dbs     map[string]*bolt.DB
	closed  bool
	lock    *sync.Mutex
}

func NewBoltShard(baseDir string) *BoltShard {
	return &BoltShard{
		baseDir: baseDir,
		closed:  false,
		dbs:     make(map[string]*bolt.DB),
		lock:    &sync.Mutex{},
	}
}

func (s *BoltShard) DropDatabase(database string) error {
	s.lock.Lock()
	defer s.lock.Unlock()

	var (
		db *bolt.DB
		ok bool
	)

	db, ok = s.dbs[database]
	if !ok {
		return nil
	}

	db.Close()
	delete(s.dbs, database)

	return os.Remove(s.baseDir + "/" + database)
}

func (s *BoltShard) close() {
	s.lock.Lock()
	defer s.lock.Unlock()

	for databaseName, db := range s.dbs {
		db.Close()
		delete(s.dbs, databaseName)
	}
	s.closed = true
}

func (s *BoltShard) IsClosed() bool {
	return s.closed
}

func (s *BoltShard) Query(querySpec *parser.QuerySpec, processor cluster.QueryProcessor) error {
	databaseName := querySpec.Database()

	var (
		db  *bolt.DB
		ok  bool
		err error
	)

	db, ok = s.dbs[databaseName]
	if !ok {
		db, err = bolt.Open(s.baseDir+"/"+databaseName, 0666)
		if err != nil {
			return err
		}

		s.dbs[databaseName] = db
	}

	// special case series
	switch {
	case querySpec.IsListSeriesQuery():
		return s.executeListSeriesQuery(db, querySpec, processor)
	case querySpec.IsDropSeriesQuery():
		return s.executeDropSeriesQuery(db, querySpec, processor)
	case querySpec.IsDeleteFromSeriesQuery():
		return s.executeDeleteFromSeriesQuery(db, querySpec, processor)
	default:
		return s.executeSeriesQuery(db, querySpec, processor)
	}

	return nil
}

func (s *BoltShard) Write(database string, series []*protocol.Series) error {
	s.lock.Lock()
	defer s.lock.Unlock()

	if s.closed {
		return errors.New("shard closed")
	}

	var err error

	var (
		db *bolt.DB
		ok bool
	)

	db, ok = s.dbs[database]
	if !ok {
		db, err = bolt.Open(s.baseDir+"/"+database, 0666)
		if err != nil {
			return err
		}

		s.dbs[database] = db
	}

	// transactional insert
	return db.Update(func(tx *bolt.Tx) error {
		var b *bolt.Bucket
		for _, serie := range series {

			seriesName := serie.GetName()
			if seriesName == "" {
				continue
			}

			b, err = tx.CreateBucketIfNotExists([]byte("series"))
			if err != nil {
				return err
			}

			b.Put([]byte(seriesName), nil)

			b, err = tx.CreateBucketIfNotExists([]byte("data"))
			if err != nil {
				return err
			}

			keyBuffer := bytes.NewBuffer(nil)
			valueBuffer := proto.NewBuffer(nil)

			for _, point := range serie.GetPoints() {
				// Each point has a timestamp and sequence number.
				timestamp := itou(point.GetTimestamp())

				// key: <series name>\x00<timestamp><sequence number>\x00<field>
				keyBuffer.Reset()
				keyBuffer.WriteString(seriesName)
				keyBuffer.WriteByte(0)

				binary.Write(keyBuffer, binary.BigEndian, &timestamp)
				binary.Write(keyBuffer, binary.BigEndian, point.SequenceNumber)

				for fieldIndex, field := range serie.Fields {
					if point.Values[fieldIndex].GetIsNull() {
						continue
					}
					valueBuffer.Reset()
					err = valueBuffer.Marshal(point.Values[fieldIndex])
					if err != nil {
						return err
					}

					fieldBucket, fieldBucketErr := tx.CreateBucketIfNotExists([]byte("fields"))
					if fieldBucketErr != nil {
						return fieldBucketErr
					}

					fieldBucketKey := []byte(seriesName + "\x00" + field)
					fieldBucket.Put(fieldBucketKey, nil)

					dataBucketKey := append(keyBuffer.Bytes(), []byte(field)...)
					b.Put(dataBucketKey, valueBuffer.Bytes())
				}
			}
		}

		return nil
	})
}
