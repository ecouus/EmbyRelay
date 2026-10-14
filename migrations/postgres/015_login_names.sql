CREATE TABLE login_names(username TEXT PRIMARY KEY, owner TEXT UNIQUE NOT NULL);
INSERT INTO login_names SELECT lower(username), 'user:'||id FROM users;
INSERT INTO login_names SELECT lower(json_extract(payload,'$.username')), 'account:'||id FROM objects WHERE kind='access_account';
CREATE FUNCTION login_users_sync() RETURNS trigger LANGUAGE plpgsql AS $$ BEGIN  IF TG_OP='DELETE' THEN DELETE FROM login_names WHERE owner='user:'||OLD.id; ELSE DELETE FROM login_names WHERE owner='user:'||NEW.id; INSERT INTO login_names VALUES(lower(NEW.username),'user:'||NEW.id); END IF; RETURN NULL; END $$;
CREATE TRIGGER login_users_sync AFTER INSERT OR UPDATE OR DELETE ON users FOR EACH ROW EXECUTE FUNCTION login_users_sync();
CREATE FUNCTION login_objects_sync() RETURNS trigger LANGUAGE plpgsql AS $$ BEGIN IF (TG_OP = 'DELETE' AND OLD.kind <> 'access_account') OR (TG_OP <> 'DELETE' AND NEW.kind <> 'access_account') THEN RETURN NULL; END IF; IF TG_OP='DELETE' THEN DELETE FROM login_names WHERE owner='account:'||OLD.id; ELSE DELETE FROM login_names WHERE owner='account:'||NEW.id; INSERT INTO login_names VALUES(lower(json_extract(NEW.payload,'$.username')),'account:'||NEW.id); END IF; RETURN NULL; END $$;
CREATE TRIGGER login_objects_sync AFTER INSERT OR UPDATE OR DELETE ON objects FOR EACH ROW EXECUTE FUNCTION login_objects_sync();
