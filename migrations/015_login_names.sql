CREATE TABLE login_names(username TEXT PRIMARY KEY, owner TEXT UNIQUE NOT NULL);
INSERT INTO login_names SELECT lower(username), 'user:'||id FROM users;
INSERT INTO login_names SELECT lower(json_extract(payload,'$.username')), 'account:'||id FROM objects WHERE kind='access_account';
CREATE TRIGGER login_users_insert AFTER INSERT ON users BEGIN DELETE FROM login_names WHERE owner='user:'||NEW.id; INSERT INTO login_names VALUES(lower(NEW.username),'user:'||NEW.id); END;
CREATE TRIGGER login_users_update AFTER UPDATE ON users BEGIN DELETE FROM login_names WHERE owner='user:'||NEW.id; INSERT INTO login_names VALUES(lower(NEW.username),'user:'||NEW.id); END;
CREATE TRIGGER login_users_delete AFTER DELETE ON users BEGIN DELETE FROM login_names WHERE owner='user:'||OLD.id; END;
CREATE TRIGGER login_objects_insert AFTER INSERT ON objects WHEN NEW.kind='access_account' BEGIN DELETE FROM login_names WHERE owner='account:'||NEW.id; INSERT INTO login_names VALUES(lower(json_extract(NEW.payload,'$.username')),'account:'||NEW.id); END;
CREATE TRIGGER login_objects_update AFTER UPDATE ON objects WHEN NEW.kind='access_account' BEGIN DELETE FROM login_names WHERE owner='account:'||NEW.id; INSERT INTO login_names VALUES(lower(json_extract(NEW.payload,'$.username')),'account:'||NEW.id); END;
CREATE TRIGGER login_objects_delete AFTER DELETE ON objects WHEN OLD.kind='access_account' BEGIN DELETE FROM login_names WHERE owner='account:'||OLD.id; END;
