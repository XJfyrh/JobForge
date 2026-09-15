-- Purpose: revert 0020 metadata; this does not delete business artifacts.
-- Lock/data risk: brief ACCESS EXCLUSIVE lock; existing references are lost.
-- Roll forward: reapply 0020 up; old references cannot be reconstructed here.
-- Verify: up/down/up on a disposable database.
alter table jobs drop column result_ref;
