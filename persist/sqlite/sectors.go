package sqlite

import (
	"database/sql"
	"errors"
	"fmt"
	"time"

	"go.sia.tech/core/types"
	"go.sia.tech/hostd/host/storage"
	"go.uber.org/zap"
)

type tempSectorRef struct {
	ID       int64
	SectorID int64
}

func deleteTempSectors(tx txn, height uint64) (sectorIDs []int64, err error) {
	const query = `DELETE FROM temp_storage_sector_roots 
WHERE id IN (SELECT id FROM temp_storage_sector_roots WHERE expiration_height <= $1 LIMIT $2)
RETURNING sector_id;`

	rows, err := tx.Query(query, height, sqlSectorBatchSize)
	if err != nil {
		return nil, fmt.Errorf("failed to select sectors: %w", err)
	}
	defer rows.Close()

	for rows.Next() {
		var sectorID int64
		if err := rows.Scan(&sectorID); err != nil {
			return nil, fmt.Errorf("failed to scan sector id: %w", err)
		}
		sectorIDs = append(sectorIDs, sectorID)
	}
	return
}

func (s *Store) batchExpireTempSectors(height uint64) (expired int, pruned []types.Hash256, err error) {
	err = s.transaction(func(tx txn) error {
		sectorIDs, err := deleteTempSectors(tx, height)
		if err != nil {
			return fmt.Errorf("failed to delete sectors: %w", err)
		} else if len(sectorIDs) == 0 {
			return nil
		}
		expired = len(sectorIDs)

		// decrement the temp sectors metric
		if err := incrementNumericStat(tx, metricTempSectors, -len(sectorIDs), time.Now()); err != nil {
			return fmt.Errorf("failed to update metric: %w", err)
		}

		pruned, err = pruneSectors(tx, sectorIDs)
		return err
	})
	return
}

// RemoveSector removes the metadata of a sector and returns its
// location in the volume.
func (s *Store) RemoveSector(root types.Hash256) (err error) {
	return s.transaction(func(tx txn) error {
		sectorID, err := sectorDBID(tx, root)
		if err != nil {
			return fmt.Errorf("failed to get sector: %w", err)
		}

		var volumeID int64
		err = tx.QueryRow(`UPDATE volume_sectors SET sector_id=null WHERE sector_id=$1 RETURNING volume_id;`, sectorID).Scan(&volumeID)
		if errors.Is(err, sql.ErrNoRows) {
			return storage.ErrSectorNotFound
		} else if err != nil {
			return fmt.Errorf("failed to remove sector: %w", err)
		}

		// decrement volume usage and metrics
		if err = incrementVolumeUsage(tx, volumeID, -1); err != nil {
			return fmt.Errorf("failed to update volume usage: %w", err)
		}
		return nil
	})
}

// SectorLocation returns the location of a sector or an error if the
// sector is not found. The sector is locked until release is
// called.
func (s *Store) SectorLocation(root types.Hash256) (storage.SectorLocation, func() error, error) {
	var lockID int64
	var location storage.SectorLocation
	err := s.transaction(func(tx txn) error {
		sectorID, err := sectorDBID(tx, root)
		if errors.Is(err, sql.ErrNoRows) {
			return storage.ErrSectorNotFound
		} else if err != nil {
			return fmt.Errorf("failed to get sector id: %w", err)
		}
		location, err = sectorLocation(tx, sectorID, root)
		if err != nil {
			return fmt.Errorf("failed to get sector location: %w", err)
		}
		lockID, err = lockSector(tx, sectorID)
		if err != nil {
			return fmt.Errorf("failed to lock sector: %w", err)
		}
		return nil
	})
	if err != nil {
		return storage.SectorLocation{}, nil, err
	}
	unlock := func() error {
		return s.transaction(func(tx txn) error {
			return unlockSector(tx, s.log.Named("SectorLocation"), lockID)
		})
	}
	return location, unlock, nil
}

// AddTemporarySectors adds the roots of sectors that are temporarily stored
// on the host. The sectors will be deleted after the expiration height.
func (s *Store) AddTemporarySectors(sectors []storage.TempSector) error {
	return s.transaction(func(tx txn) error {
		stmt, err := tx.Prepare(`INSERT INTO temp_storage_sector_roots (sector_id, expiration_height) SELECT id, $1 FROM stored_sectors WHERE sector_root=$2 RETURNING id;`)
		if err != nil {
			return fmt.Errorf("failed to prepare query: %w", err)
		}
		defer stmt.Close()
		for _, sector := range sectors {
			var dbID int64
			err := stmt.QueryRow(sector.Expiration, sqlHash256(sector.Root)).Scan(&dbID)
			if err != nil {
				return fmt.Errorf("failed to add temp sector root: %w", err)
			}
		}
		if err := incrementNumericStat(tx, metricTempSectors, len(sectors), time.Now()); err != nil {
			return fmt.Errorf("failed to update metric: %w", err)
		}
		return nil
	})
}

// ExpireTempSectors deletes the roots of sectors that are no longer
// temporarily stored on the host.
func (s *Store) ExpireTempSectors(height uint64) error {
	log := s.log.Named("ExpireTempSectors").With(zap.Uint64("height", height))
	// delete in batches to avoid holding a lock on the table for too long
	for i := 0; ; i++ {
		expired, removed, err := s.batchExpireTempSectors(height)
		if err != nil {
			return fmt.Errorf("failed to expire sectors: %w", err)
		} else if expired == 0 {
			return nil
		}
		log.Debug("expired temp sectors", zap.Int("expired", expired), zap.Stringers("removed", removed), zap.Int("batch", i))
		jitterSleep(time.Millisecond) // allow other transactions to run
	}
}

// HasSector returns true if the sector is stored on the host.
func (s *Store) HasSector(root types.Hash256) (bool, error) {
	var dbID int64
	err := s.queryRow(`SELECT id FROM stored_sectors WHERE sector_root=$1`, sqlHash256(root)).Scan(&dbID)
	if errors.Is(err, sql.ErrNoRows) {
		return false, nil
	} else if err != nil {
		return false, err
	}
	return true, nil
}

// SectorReferences returns the references, if any of a sector root
func (s *Store) SectorReferences(root types.Hash256) (refs storage.SectorReference, err error) {
	err = s.transaction(func(tx txn) error {
		dbID, err := sectorDBID(tx, root)
		if err != nil {
			return fmt.Errorf("failed to get sector id: %w", err)
		}

		// check if the sector is referenced by a contract
		refs.Contracts, err = contractSectorRefs(tx, dbID)
		if err != nil {
			return fmt.Errorf("failed to get contracts: %w", err)
		}

		// check if the sector is referenced by temp storage
		refs.TempStorage, err = getTempStorageCount(tx, dbID)
		if err != nil {
			return fmt.Errorf("failed to get temp storage: %w", err)
		}

		// check if the sector is locked
		refs.Locks, err = getSectorLockCount(tx, dbID)
		if err != nil {
			return fmt.Errorf("failed to get locks: %w", err)
		}
		return nil
	})
	return
}

func contractSectorRefs(tx txn, sectorID int64) (contractIDs []types.FileContractID, err error) {
	rows, err := tx.Query(`SELECT DISTINCT contract_id FROM contract_sector_roots WHERE sector_id=$1;`, sectorID)
	if err != nil {
		return nil, fmt.Errorf("failed to select contracts: %w", err)
	}
	defer rows.Close()

	for rows.Next() {
		var contractID types.FileContractID
		if err := rows.Scan((*sqlHash256)(&contractID)); err != nil {
			return nil, fmt.Errorf("failed to scan contract id: %w", err)
		}
		contractIDs = append(contractIDs, contractID)
	}
	return
}

func getTempStorageCount(tx txn, sectorID int64) (n int, err error) {
	err = tx.QueryRow(`SELECT COUNT(*) FROM temp_storage_sector_roots WHERE sector_id=$1;`, sectorID).Scan(&n)
	return
}

func getSectorLockCount(tx txn, sectorID int64) (n int, err error) {
	err = tx.QueryRow(`SELECT COUNT(*) FROM locked_sectors WHERE sector_id=$1;`, sectorID).Scan(&n)
	return
}

func incrementVolumeUsage(tx txn, volumeID int64, delta int) error {
	var used int64
	err := tx.QueryRow(`UPDATE storage_volumes SET used_sectors=used_sectors+$1 WHERE id=$2 RETURNING used_sectors;`, delta, volumeID).Scan(&used)
	if err != nil {
		return fmt.Errorf("failed to update volume: %w", err)
	} else if used < 0 {
		panic("volume usage is negative") // developer error
	} else if err = incrementNumericStat(tx, metricPhysicalSectors, delta, time.Now()); err != nil {
		return fmt.Errorf("failed to update metric: %w", err)
	}
	return nil
}

func pruneSectors(tx txn, ids []int64) (pruned []types.Hash256, err error) {
	hasContractRefStmt, err := tx.Prepare(`SELECT id FROM contract_sector_roots WHERE sector_id=$1 LIMIT 1`)
	if err != nil {
		return nil, fmt.Errorf("failed to prepare contract reference query: %w", err)
	}
	defer hasContractRefStmt.Close()

	hasTempRefStmt, err := tx.Prepare(`SELECT id FROM temp_storage_sector_roots WHERE sector_id=$1 LIMIT 1`)
	if err != nil {
		return nil, fmt.Errorf("failed to prepare temp reference query: %w", err)
	}
	defer hasTempRefStmt.Close()

	hasLockStmt, err := tx.Prepare(`SELECT id FROM locked_sectors WHERE sector_id=$1 LIMIT 1`)
	if err != nil {
		return nil, fmt.Errorf("failed to prepare lock reference query: %w", err)
	}
	defer hasLockStmt.Close()

	clearVolumeStmt, err := tx.Prepare(`UPDATE volume_sectors SET sector_id=NULL WHERE sector_id=$1 RETURNING volume_id`)
	if err != nil {
		return nil, fmt.Errorf("failed to prepare volume reference query: %w", err)
	}
	defer clearVolumeStmt.Close()

	deleteSectorStmt, err := tx.Prepare(`DELETE FROM stored_sectors WHERE id=$1 RETURNING sector_root`)
	if err != nil {
		return nil, fmt.Errorf("failed to prepare delete sector query: %w", err)
	}
	defer deleteSectorStmt.Close()

	volumeDelta := make(map[int64]int)
	for _, id := range ids {
		var contractDBID int64
		err := hasContractRefStmt.QueryRow(id).Scan(&contractDBID)
		if err == nil {
			continue // sector has a contract reference
		} else if err != nil && !errors.Is(err, sql.ErrNoRows) {
			return nil, fmt.Errorf("failed to check contract references: %w", err)
		}

		var tempDBID int64
		err = hasTempRefStmt.QueryRow(id).Scan(&tempDBID)
		if err == nil {
			continue // sector has a temp storage reference
		} else if err != nil && !errors.Is(err, sql.ErrNoRows) {
			return nil, fmt.Errorf("failed to check temp references: %w", err)
		}

		var lockDBID int64
		err = hasLockStmt.QueryRow(id).Scan(&lockDBID)
		if err == nil {
			continue // sector is locked
		} else if err != nil && !errors.Is(err, sql.ErrNoRows) {
			return nil, fmt.Errorf("failed to check lock references: %w", err)
		}

		var volumeDBID int64
		err = clearVolumeStmt.QueryRow(id).Scan(&volumeDBID)
		if err == nil {
			volumeDelta[volumeDBID]-- // sector was removed from a volume
		} else if err != nil && !errors.Is(err, sql.ErrNoRows) { // ignore rows not found
			return nil, fmt.Errorf("failed to clear volume references: %w", err)
		}

		var root types.Hash256
		err = deleteSectorStmt.QueryRow(id).Scan((*sqlHash256)(&root))
		if err == nil {
			pruned = append(pruned, root)
		} else if err != nil && !errors.Is(err, sql.ErrNoRows) { // ignore rows not found
			return nil, fmt.Errorf("failed to delete sector: %w", err)
		}
	}

	// decrement the usage of all changed volumes
	for volumeDBID, delta := range volumeDelta {
		if err := incrementVolumeUsage(tx, volumeDBID, delta); err != nil {
			return nil, fmt.Errorf("failed to update volume usage: %w", err)
		}
	}
	return
}

func expiredTempSectors(tx txn, height uint64, limit int) (sectors []tempSectorRef, _ error) {
	const query = `SELECT ts.id, ts.sector_id FROM temp_storage_sector_roots ts
WHERE expiration_height <= $1 LIMIT $2;`
	rows, err := tx.Query(query, height, limit)
	if err != nil {
		return nil, fmt.Errorf("failed to select sectors: %w", err)
	}
	defer rows.Close()
	for rows.Next() {
		var ref tempSectorRef
		if err := rows.Scan(&ref.ID, &ref.SectorID); err != nil {
			return nil, fmt.Errorf("failed to scan sector id: %w", err)
		}
		sectors = append(sectors, ref)
	}
	return
}

// lockSector locks a sector root. The lock must be released by calling
// unlockSector. A sector must be locked when it is being read or written
// to prevent it from being removed by prune sector.
func lockSector(tx txn, sectorDBID int64) (lockID int64, err error) {
	err = tx.QueryRow(`INSERT INTO locked_sectors (sector_id) VALUES ($1) RETURNING id;`, sectorDBID).Scan(&lockID)
	return
}

// deleteLocks removes the lock records with the given ids and returns the
// sector ids of the deleted locks.
func deleteLocks(tx txn, ids []int64) (sectorIDs []int64, err error) {
	if len(ids) == 0 {
		return nil, nil
	}

	query := `DELETE FROM locked_sectors WHERE id IN (` + queryPlaceHolders(len(ids)) + `) RETURNING sector_id;`
	rows, err := tx.Query(query, queryArgs(ids)...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	for rows.Next() {
		var sectorID int64
		if err := rows.Scan(&sectorID); err != nil {
			return nil, fmt.Errorf("failed to scan sector id: %w", err)
		}
		sectorIDs = append(sectorIDs, sectorID)
	}
	return
}

// unlockSector unlocks a sector root.
func unlockSector(txn txn, log *zap.Logger, lockIDs ...int64) error {
	if len(lockIDs) == 0 {
		return nil
	}

	sectorIDs, err := deleteLocks(txn, lockIDs)
	if err != nil {
		return fmt.Errorf("failed to delete locks: %w", err)
	}

	pruned, err := pruneSectors(txn, sectorIDs)
	if err != nil {
		return fmt.Errorf("failed to prune sectors: %w", err)
	}
	log.Debug("unlocked sectors", zap.Int("unlocked", len(lockIDs)), zap.Stringers("removed", pruned))
	return nil
}

// lockLocations locks multiple sector locations and returns a list of lock
// IDs. The lock ids must be unlocked by unlockLocations. Volume locations
// should be locked during writes to prevent the location from being written
// to by another goroutine.
func lockLocations(tx txn, locations []storage.SectorLocation) (locks []int64, err error) {
	if len(locations) == 0 {
		return nil, nil
	}
	stmt, err := tx.Prepare(`INSERT INTO locked_volume_sectors (volume_sector_id) VALUES ($1) RETURNING id;`)
	if err != nil {
		return nil, fmt.Errorf("failed to prepare query: %w", err)
	}
	defer stmt.Close()
	for _, location := range locations {
		var lockID int64
		err := stmt.QueryRow(location.ID).Scan(&lockID)
		if err != nil {
			return nil, fmt.Errorf("failed to lock location %v:%v: %w", location.Volume, location.Index, err)
		}
		locks = append(locks, lockID)
	}
	return
}

// unlockLocations unlocks multiple locked sector locations. It is safe to
// call multiple times.
func unlockLocations(tx txn, ids []int64) error {
	if len(ids) == 0 {
		return nil
	}

	query := `DELETE FROM locked_volume_sectors WHERE id IN (` + queryPlaceHolders(len(ids)) + `);`
	_, err := tx.Exec(query, queryArgs(ids)...)
	return err
}
