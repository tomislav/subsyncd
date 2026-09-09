-- Track titles were not retained, so old forced classifications cannot be
-- repaired from stored tracks. Reprobe once on each file's next normal search.
-- Preserve inventory, ownership, schedules and leases; new probes remain cached.
DELETE FROM inventory_probes;
