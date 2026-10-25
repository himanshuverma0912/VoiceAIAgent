-- Each flow belongs to one team (a Keycloak group /teams/<name>). Only that
-- team's editors and publishers can change it. Flows created before teams
-- existed have NULL until a platform admin assigns them.
ALTER TABLE flows ADD COLUMN team text CHECK (team ~ '^[a-z][a-z0-9_-]{0,31}$');
CREATE INDEX flows_team ON flows (team);
