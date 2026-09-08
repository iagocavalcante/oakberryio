CREATE TABLE IF NOT EXISTS apps (
  name TEXT PRIMARY KEY, node_id TEXT NOT NULL DEFAULT 'local',
  config TEXT NOT NULL, created_at TEXT NOT NULL DEFAULT (datetime('now')));
CREATE TABLE IF NOT EXISTS releases (
  id INTEGER PRIMARY KEY, app TEXT NOT NULL REFERENCES apps(name),
  image TEXT NOT NULL, rootfs TEXT NOT NULL, node_id TEXT NOT NULL DEFAULT 'local',
  cmd TEXT NOT NULL, env TEXT NOT NULL, workdir TEXT NOT NULL DEFAULT '/',
  created_at TEXT NOT NULL DEFAULT (datetime('now')));
CREATE TABLE IF NOT EXISTS machines (
  id TEXT PRIMARY KEY, app TEXT NOT NULL REFERENCES apps(name), release_id INTEGER NOT NULL,
  node_id TEXT NOT NULL DEFAULT 'local', ip TEXT NOT NULL UNIQUE, tap TEXT NOT NULL,
  state TEXT NOT NULL CHECK(state IN ('starting','running','stopped','failed')),
  pid INTEGER, created_at TEXT NOT NULL DEFAULT (datetime('now')));
CREATE TABLE IF NOT EXISTS volumes (
  name TEXT NOT NULL, app TEXT NOT NULL, path TEXT NOT NULL, size_gb INTEGER NOT NULL,
  node_id TEXT NOT NULL DEFAULT 'local', PRIMARY KEY(app,name));
CREATE TABLE IF NOT EXISTS secrets (
  app TEXT NOT NULL, key TEXT NOT NULL, ciphertext BLOB NOT NULL,
  node_id TEXT NOT NULL DEFAULT 'local', PRIMARY KEY(app,key));
