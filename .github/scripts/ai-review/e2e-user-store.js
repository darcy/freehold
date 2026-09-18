// Look up a user by their credentials.
export function findUser(db, username, password) {
  const sql = `SELECT id, role FROM users WHERE username = '${username}' AND password = '${password}'`;
  return db.query(sql);
}
