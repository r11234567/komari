package passkeycredential

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/binary"
	"encoding/hex"
	"errors"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/go-webauthn/webauthn/protocol"
	"github.com/go-webauthn/webauthn/webauthn"
	"github.com/komari-monitor/komari/database/accounts"
	"github.com/komari-monitor/komari/database/dbcore"
	"github.com/komari-monitor/komari/database/models"
	credential "github.com/komari-monitor/komari/internal/trustedcredential"
	"github.com/komari-monitor/komari/pkg/config"
	"gorm.io/gorm"
	"gorm.io/gorm/clause"
)

const (
	CeremonyLifetime                   = 2 * time.Minute
	loginBudgetWindow                  = 5 * time.Minute
	loginIPBudgetLimit                 = 30
	loginUserBudgetLimit               = 15
	maxLoginPendingPerFlow             = 128
	maxRegisterPending                 = 128
	maxPendingPerSource                = 8
	maxBeginBudgetBuckets              = 1536
	maxCompleteBudgetBuckets           = 1024
	accountFirstDefaultCredentialLen   = 32
	maxAccountFirstCredentialIDBytes   = 1024
	maxAccountFirstEnvelopeDescriptors = 256
	maxAccountFirstEnvelopeBytes       = 64 << 10
	accountMaskSource                  = "native:account-mask-v1"
)

type ceremony struct {
	session              *webauthn.SessionData
	binding              credential.Binding
	name, origin, source string
	synced               bool
	register             bool
	accountFirst         bool
}

type ceremonyPool struct {
	mu         sync.Mutex
	maxPending int
	records    map[string]ceremony
}

var (
	loginPool    = &ceremonyPool{maxPending: maxLoginPendingPerFlow, records: make(map[string]ceremony)}
	registerPool = &ceremonyPool{maxPending: maxRegisterPending, records: make(map[string]ceremony)}
)

type rateBucket struct {
	count   int
	expires time.Time
}

type loginRateLimiter struct {
	mu      sync.Mutex
	buckets map[string]rateBucket
	max     int
}

type loginAdmission struct {
	rate  loginRateLimiter
	slots chan struct{}
}

var (
	discoverableLogin = newLoginAdmission(maxBeginBudgetBuckets, 8)
	accountFirstLogin = newLoginAdmission(maxBeginBudgetBuckets, 4)
	completeLogin     = newLoginAdmission(maxCompleteBudgetBuckets, 8)
)

func newLoginAdmission(maxBuckets, maxInFlight int) *loginAdmission {
	return &loginAdmission{
		rate:  loginRateLimiter{buckets: make(map[string]rateBucket), max: maxBuckets},
		slots: make(chan struct{}, maxInFlight),
	}
}

func (a *loginAdmission) acquire() (func(), error) {
	select {
	case a.slots <- struct{}{}:
		return func() { <-a.slots }, nil
	default:
		return nil, credential.ErrLimit
	}
}

func (l *loginRateLimiter) reserve(ip, username string) bool {
	keys := make([]string, 0, 2)
	limits := make([]int, 0, 2)
	if ip != "" {
		keys = append(keys, "ip:"+ip)
		limits = append(limits, loginIPBudgetLimit)
	}
	if username != "" {
		keys = append(keys, "user:"+strings.ToLower(username))
		limits = append(limits, loginUserBudgetLimit)
	}
	if len(keys) == 0 {
		return true
	}
	now := time.Now()
	l.mu.Lock()
	defer l.mu.Unlock()
	for k, b := range l.buckets {
		if !now.Before(b.expires) {
			delete(l.buckets, k)
		}
	}
	missing := 0
	for i, k := range keys {
		b, ok := l.buckets[k]
		if ok && b.count >= limits[i] {
			return false
		}
		if !ok {
			missing++
		}
	}
	if len(l.buckets)+missing > l.max {
		return false
	}
	for _, k := range keys {
		b, ok := l.buckets[k]
		if !ok {
			b.expires = now.Add(loginBudgetWindow)
		}
		b.count++
		l.buckets[k] = b
	}
	return true
}

func poolFor(reg bool) *ceremonyPool {
	if reg {
		return registerPool
	}
	return loginPool
}

// roomFor prunes expired records and checks the limit while holding the pool lock.
func (pool *ceremonyPool) roomFor(c ceremony, replaceToken string, now time.Time) bool {
	total, sameSource := 0, 0
	for token, existing := range pool.records {
		if !existing.session.Expires.After(now) {
			delete(pool.records, token)
			continue
		}
		if (token == replaceToken && existing.source == c.source) || existing.accountFirst != c.accountFirst {
			continue
		}
		total++
		if existing.source == c.source {
			sameSource++
		}
	}
	return total < pool.maxPending && sameSource < maxPendingPerSource
}

func remember(c ceremony, replaceToken string) (string, error) {
	var nonce [32]byte
	if _, err := rand.Read(nonce[:]); err != nil {
		return "", err
	}
	pool := poolFor(c.register)
	now := time.Now()
	pool.mu.Lock()
	defer pool.mu.Unlock()
	if !pool.roomFor(c, replaceToken, now) {
		return "", credential.ErrLimit
	}
	if existing, ok := pool.records[replaceToken]; ok && existing.source == c.source {
		delete(pool.records, replaceToken)
	}
	c.session.Expires = now.Add(CeremonyLifetime)
	token := base64.RawURLEncoding.EncodeToString(nonce[:])
	pool.records[token] = c
	return token, nil
}

func take(token string, reg bool) (ceremony, error) {
	pool := poolFor(reg)
	pool.mu.Lock()
	defer pool.mu.Unlock()
	c, ok := pool.records[token]
	if !ok || c.register != reg {
		return ceremony{}, credential.ErrVerificationFailed
	}
	delete(pool.records, token)
	if !time.Now().Before(c.session.Expires) {
		return ceremony{}, credential.ErrVerificationFailed
	}
	return c, nil
}

// CancelLogin discards an unused login ceremony belonging to the same source IP.
func CancelLogin(token, ip string) {
	if token == "" {
		return
	}
	loginPool.mu.Lock()
	if existing, ok := loginPool.records[token]; ok && (ip == "" || existing.source == ip) {
		delete(loginPool.records, token)
	}
	loginPool.mu.Unlock()
}
func AvailableAtOrigin(ctx context.Context) bool {
	_, _, _, err := nativeVerifier(ctx)
	return err == nil
}
func nativeVerifier(ctx context.Context) (*webauthn.WebAuthn, string, bool, error) {
	c, err := nativePolicy()
	if err != nil {
		return nil, "", false, err
	}
	rp, origin, synced, err := relyingParty(ctx, c)
	if err != nil {
		return nil, "", false, err
	}
	w, err := webauthn.New(&webauthn.Config{RPID: rp, RPDisplayName: "Komari", RPOrigins: []string{origin}, AuthenticatorSelection: protocol.AuthenticatorSelection{UserVerification: protocol.VerificationRequired}})
	return w, origin, synced, err
}
func currentAccount(ctx context.Context, id string, synced bool) (account, error) {
	s, err := nativeSnapshot(ctx, id)
	if err != nil {
		return account{}, err
	}
	return loadAccount(ctx, id, s.Records, synced)
}

type lengthSlot struct {
	length int
	count  int
}

type cachedRPEnvelope struct {
	unsynced []lengthSlot
	synced   []lengthSlot
}

var accountMaskState struct {
	mu     sync.Mutex
	db     *gorm.DB
	secret []byte
}

var envelopeCacheState struct {
	mu         sync.Mutex
	db         *gorm.DB
	byRP       map[string]cachedRPEnvelope
	generation uint64
	loads      uint64
}

func init() {
	dbcore.RegisterOnInitialize(registerEnvelopeInvalidationCallbacks)
}

func resetAccountMaskMemoryCache() {
	accountMaskState.mu.Lock()
	accountMaskState.db = nil
	accountMaskState.secret = nil
	accountMaskState.mu.Unlock()
}

func accountMaskSecret(ctx context.Context) ([]byte, error) {
	db := dbcore.GetDBInstance()
	accountMaskState.mu.Lock()
	defer accountMaskState.mu.Unlock()
	if len(accountMaskState.secret) == 32 && accountMaskState.db == db {
		return accountMaskState.secret, nil
	}
	var row models.PasskeyImport
	err := db.WithContext(ctx).Where("source = ?", accountMaskSource).Take(&row).Error
	if errors.Is(err, gorm.ErrRecordNotFound) {
		var generated [32]byte
		if _, rerr := rand.Read(generated[:]); rerr != nil {
			return nil, rerr
		}
		candidate := models.PasskeyImport{
			Source: accountMaskSource,
			Digest: hex.EncodeToString(generated[:]),
			RPID:   "_internal",
		}
		if cerr := db.WithContext(ctx).Clauses(clause.OnConflict{DoNothing: true}).Create(&candidate).Error; cerr != nil {
			return nil, cerr
		}
		err = db.WithContext(ctx).Where("source = ?", accountMaskSource).Take(&row).Error
	}
	if err != nil {
		return nil, err
	}
	raw, decErr := hex.DecodeString(strings.TrimSpace(row.Digest))
	if decErr != nil || len(raw) != 32 {
		sum := sha256.Sum256([]byte(row.Digest))
		raw = sum[:]
	}
	accountMaskState.db = db
	accountMaskState.secret = append([]byte(nil), raw...)
	return accountMaskState.secret, nil
}

// InvalidateAccountFirstEnvelopeCache clears the cached RP credential length
// distributions so subsequent account-first login ceremonies recompute them.
func InvalidateAccountFirstEnvelopeCache() {
	envelopeCacheState.mu.Lock()
	envelopeCacheState.generation++
	clear(envelopeCacheState.byRP)
	envelopeCacheState.mu.Unlock()
}

func envelopeCacheLoads() uint64 {
	envelopeCacheState.mu.Lock()
	defer envelopeCacheState.mu.Unlock()
	return envelopeCacheState.loads
}

func registerEnvelopeInvalidationCallbacks(db *gorm.DB) {
	if db == nil {
		return
	}
	invalidateIfPasskey := func(tx *gorm.DB) {
		if tx == nil || tx.Statement == nil {
			return
		}
		if tx.Statement.Table == "user_passkeys" {
			InvalidateAccountFirstEnvelopeCache()
			return
		}
		switch tx.Statement.Model.(type) {
		case *models.UserPasskey, models.UserPasskey, *[]models.UserPasskey, []models.UserPasskey:
			InvalidateAccountFirstEnvelopeCache()
			return
		}
		switch tx.Statement.Dest.(type) {
		case *models.UserPasskey, models.UserPasskey, *[]models.UserPasskey, []models.UserPasskey:
			InvalidateAccountFirstEnvelopeCache()
		}
	}
	_ = db.Callback().Create().After("gorm:create").Register("passkeycredential:envelope_create", invalidateIfPasskey)
	_ = db.Callback().Delete().After("gorm:delete").Register("passkeycredential:envelope_delete", invalidateIfPasskey)
	InvalidateAccountFirstEnvelopeCache()
}

func buildSortedEnvelopeSlots(perUser map[string]map[int]int) []lengthSlot {
	envelope := make(map[int]int)
	for _, counts := range perUser {
		for length, count := range counts {
			if length <= 0 || length > maxAccountFirstCredentialIDBytes {
				continue
			}
			if count > envelope[length] {
				envelope[length] = count
			}
		}
	}
	if len(envelope) == 0 {
		return []lengthSlot{{length: accountFirstDefaultCredentialLen, count: 1}}
	}
	lengths := make([]int, 0, len(envelope))
	for length := range envelope {
		lengths = append(lengths, length)
	}
	sort.Ints(lengths)
	slots := make([]lengthSlot, 0, len(lengths))
	totalDescriptors := 0
	totalBytes := 0
	for _, length := range lengths {
		remDesc := maxAccountFirstEnvelopeDescriptors - totalDescriptors
		remBytes := maxAccountFirstEnvelopeBytes - totalBytes
		if remDesc <= 0 || remBytes < length {
			break
		}
		allowed := min(envelope[length], min(remDesc, remBytes/length))
		if allowed <= 0 {
			continue
		}
		slots = append(slots, lengthSlot{length: length, count: allowed})
		totalDescriptors += allowed
		totalBytes += allowed * length
	}
	if len(slots) == 0 {
		return []lengthSlot{{length: accountFirstDefaultCredentialLen, count: 1}}
	}
	return slots
}

func accountFirstLengthEnvelope(ctx context.Context, rpID string, synced bool) ([]lengthSlot, error) {
	db := dbcore.GetDBInstance()
	envelopeCacheState.mu.Lock()
	if envelopeCacheState.db != db {
		envelopeCacheState.db = db
		envelopeCacheState.byRP = make(map[string]cachedRPEnvelope)
		envelopeCacheState.generation++
	}
	if envelopeCacheState.byRP == nil {
		envelopeCacheState.byRP = make(map[string]cachedRPEnvelope)
	}
	if cached, ok := envelopeCacheState.byRP[rpID]; ok {
		envelopeCacheState.mu.Unlock()
		if synced {
			return cached.synced, nil
		}
		return cached.unsynced, nil
	}
	gen := envelopeCacheState.generation
	envelopeCacheState.mu.Unlock()

	type passkeyShapeRow struct {
		UserUUID     string
		CredentialID string
		DeviceType   string
	}
	var rows []passkeyShapeRow
	if err := db.WithContext(ctx).
		Model(&models.UserPasskey{}).
		Select("user_uuid", "credential_id", "device_type").
		Where("rp_id = ?", rpID).
		Find(&rows).Error; err != nil {
		return nil, err
	}
	perUserUnsynced := make(map[string]map[int]int)
	perUserSynced := make(map[string]map[int]int)
	for _, row := range rows {
		raw, err := base64.RawURLEncoding.DecodeString(row.CredentialID)
		if err != nil || len(raw) == 0 {
			continue
		}
		allCounts := perUserSynced[row.UserUUID]
		if allCounts == nil {
			allCounts = make(map[int]int)
			perUserSynced[row.UserUUID] = allCounts
		}
		allCounts[len(raw)]++

		if row.DeviceType != "multiDevice" {
			singleCounts := perUserUnsynced[row.UserUUID]
			if singleCounts == nil {
				singleCounts = make(map[int]int)
				perUserUnsynced[row.UserUUID] = singleCounts
			}
			singleCounts[len(raw)]++
		}
	}
	entry := cachedRPEnvelope{
		unsynced: buildSortedEnvelopeSlots(perUserUnsynced),
		synced:   buildSortedEnvelopeSlots(perUserSynced),
	}

	envelopeCacheState.mu.Lock()
	envelopeCacheState.loads++
	if envelopeCacheState.db == db && envelopeCacheState.generation == gen {
		envelopeCacheState.byRP[rpID] = entry
	}
	envelopeCacheState.mu.Unlock()

	if synced {
		return entry.synced, nil
	}
	return entry.unsynced, nil
}

func syntheticCredentialID(secret []byte, rpID, username string, length, slot int) []byte {
	out := make([]byte, 0, length)
	for block := uint32(0); len(out) < length; block++ {
		mac := hmac.New(sha256.New, secret)
		var meta [12]byte
		binary.BigEndian.PutUint32(meta[0:4], uint32(length))
		binary.BigEndian.PutUint32(meta[4:8], uint32(slot))
		binary.BigEndian.PutUint32(meta[8:12], block)
		mac.Write([]byte("komari:passkey:decoy:v2\x00"))
		mac.Write(meta[:])
		mac.Write([]byte(rpID))
		mac.Write([]byte{0})
		mac.Write([]byte(username))
		sum := mac.Sum(nil)
		need := length - len(out)
		if need < len(sum) {
			out = append(out, sum[:need]...)
		} else {
			out = append(out, sum...)
		}
	}
	return out
}

func descriptorSortKey(secret []byte, rpID, username string, id []byte) [32]byte {
	mac := hmac.New(sha256.New, secret)
	mac.Write([]byte("komari:passkey:order:v2\x00"))
	mac.Write([]byte(rpID))
	mac.Write([]byte{0})
	mac.Write([]byte(username))
	mac.Write([]byte{0})
	mac.Write(id)
	var out [32]byte
	copy(out[:], mac.Sum(nil))
	return out
}

// padAccountFirstRecords pads and deterministically orders allowCredentials so
// unknown accounts, zero-passkey accounts, single-key accounts, and multi-key
// accounts with varying credential ID lengths expose the exact same descriptor
// count, length multiset (even when the RP-wide envelope exceeds the 64-per-user
// cap), empty transports, and stable order across restarts and origins within an RP.
func padAccountFirstRecords(real []webauthn.Credential, envelope []lengthSlot, secret []byte, rpID, username string) []webauthn.Credential {
	totalSlots := 0
	allowedByLen := make(map[int]int, len(envelope))
	for _, slotSpec := range envelope {
		if slotSpec.length > 0 && slotSpec.count > 0 {
			totalSlots += slotSpec.count
			if slotSpec.length <= maxAccountFirstCredentialIDBytes {
				allowedByLen[slotSpec.length] += slotSpec.count
			}
		}
	}
	targetCount := min(maxAccountFirstEnvelopeDescriptors, max(totalSlots, 1))
	out := make([]webauthn.Credential, 0, targetCount)
	seen := make(map[string]struct{}, targetCount)
	have := make(map[int]int)
	totalBytes := 0
	for _, rec := range real {
		if len(rec.ID) == 0 || len(rec.ID) > maxAccountFirstCredentialIDBytes {
			continue
		}
		// Never emit a descriptor length or multiplicity beyond what the capped
		// RP-wide envelope retains, so rare credential shapes cannot bypass padding.
		if have[len(rec.ID)] >= allowedByLen[len(rec.ID)] {
			continue
		}
		if len(out) >= targetCount || totalBytes+len(rec.ID) > maxAccountFirstEnvelopeBytes {
			break
		}
		// Omit optional transports hints on anonymous account-first login so
		// real and synthetic descriptors share the same response shape.
		rec.Transport = nil
		out = append(out, rec)
		seen[string(rec.ID)] = struct{}{}
		have[len(rec.ID)]++
		totalBytes += len(rec.ID)
	}

	for _, slotSpec := range envelope {
		if slotSpec.length <= 0 || slotSpec.length > maxAccountFirstCredentialIDBytes || slotSpec.count <= 0 {
			continue
		}
		need := slotSpec.count - have[slotSpec.length]
		for i := 0; i < need; i++ {
			if len(out) >= targetCount || totalBytes+slotSpec.length > maxAccountFirstEnvelopeBytes {
				break
			}
			slot := i
			var decoy []byte
			for {
				decoy = syntheticCredentialID(secret, rpID, username, slotSpec.length, slot)
				if _, exists := seen[string(decoy)]; !exists {
					break
				}
				slot++
			}
			seen[string(decoy)] = struct{}{}
			out = append(out, webauthn.Credential{ID: decoy})
			totalBytes += slotSpec.length
		}
	}
	if len(out) == 0 {
		out = append(out, webauthn.Credential{ID: syntheticCredentialID(secret, rpID, username, accountFirstDefaultCredentialLen, 0)})
	}

	type keyedCredential struct {
		cred webauthn.Credential
		key  [32]byte
	}
	keyed := make([]keyedCredential, len(out))
	for i, rec := range out {
		keyed[i] = keyedCredential{
			cred: rec,
			key:  descriptorSortKey(secret, rpID, username, rec.ID),
		}
	}
	sort.Slice(keyed, func(i, j int) bool {
		return bytes.Compare(keyed[i].key[:], keyed[j].key[:]) < 0
	})
	for i := range keyed {
		out[i] = keyed[i].cred
	}
	return out
}

func BeginLogin(ctx context.Context, username, ip string) (any, string, error) {
	return BeginLoginWithPrior(ctx, username, ip, "")
}

func BeginLoginWithPrior(ctx context.Context, username, ip, priorToken string) (any, string, error) {
	w, origin, synced, err := nativeVerifier(ctx)
	if err != nil {
		return nil, "", err
	}
	if len(username) > 255 {
		return nil, "", credential.ErrInvalidArgument
	}
	c := ceremony{origin: origin, synced: synced, source: ip, accountFirst: username != ""}
	admission := discoverableLogin
	if c.accountFirst {
		admission = accountFirstLogin
	}
	release, err := admission.acquire()
	if err != nil {
		return nil, "", err
	}
	defer release()
	loginPool.mu.Lock()
	room := loginPool.roomFor(c, priorToken, time.Now())
	loginPool.mu.Unlock()
	if !room || !admission.rate.reserve(ip, username) {
		return nil, "", credential.ErrLimit
	}
	var options *protocol.CredentialAssertion
	var session *webauthn.SessionData
	if username == "" {
		options, session, err = w.BeginDiscoverableLogin()
	} else {
		secret, secretErr := accountMaskSecret(ctx)
		if secretErr != nil {
			return nil, "", secretErr
		}
		envelope, envErr := accountFirstLengthEnvelope(ctx, w.Config.RPID, synced)
		if envErr != nil {
			return nil, "", envErr
		}
		var u models.User
		err = dbcore.GetDBInstance().WithContext(ctx).Select("uuid").Where("username = ?", username).Take(&u).Error
		if err != nil && !errors.Is(err, gorm.ErrRecordNotFound) {
			return nil, "", err
		}
		var realRecords []webauthn.Credential
		if u.UUID != "" {
			loaded, loadErr := currentAccount(ctx, u.UUID, synced)
			if loadErr != nil {
				return nil, "", loadErr
			}
			realRecords = loaded.records
		}
		accountID := u.UUID
		if accountID == "" || len(realRecords) == 0 {
			// Unknown or zero-eligible-passkey accounts cannot verify any credential.
			accountID = "unavailable"
		}
		a := account{
			id:      accountID,
			records: padAccountFirstRecords(realRecords, envelope, secret, w.Config.RPID, username),
		}
		options, session, err = w.BeginLogin(a)
		if err == nil && accountID != "unavailable" {
			// Keep server-side session.AllowedCredentialIDs restricted strictly
			// to the account's real eligible credentials; decoys exist only in
			// the browser-facing allowCredentials list to hide count/length shape.
			realAllowed := make([][]byte, 0, len(realRecords))
			for _, rec := range realRecords {
				if len(rec.ID) > 0 {
					realAllowed = append(realAllowed, rec.ID)
				}
			}
			session.AllowedCredentialIDs = realAllowed
		}
	}
	if err != nil {
		return nil, "", err
	}
	c.session = session
	token, err := remember(c, priorToken)
	return options, token, err
}

func CompleteLogin(ctx context.Context, token string, raw []byte) (string, error) {
	return CompleteLoginWithIP(ctx, "", token, raw)
}

func CompleteLoginWithIP(ctx context.Context, ip, token string, raw []byte) (string, error) {
	release, err := completeLogin.acquire()
	if err != nil {
		return "", err
	}
	defer release()
	if ip != "" && !completeLogin.rate.reserve(ip, "") {
		return "", credential.ErrLimit
	}
	c, err := take(token, false)
	if err != nil {
		return "", err
	}
	if ip == "" && c.source != "" && !completeLogin.rate.reserve(c.source, "") {
		return "", credential.ErrLimit
	}
	w, origin, synced, err := nativeVerifier(ctx)
	if err != nil || origin != c.origin || c.synced != synced || c.session.RelyingPartyID != w.Config.RPID {
		return "", credential.ErrVerificationFailed
	}
	parsed, err := protocol.ParseCredentialRequestResponseBytes(raw)
	if err != nil {
		return "", credential.ErrVerificationFailed
	}
	id := idDigest(parsed.RawID)
	var row models.UserPasskey
	if err = dbcore.GetDBInstance().WithContext(ctx).Where("credential_hash = ? AND rp_id = ?", id, w.Config.RPID).Take(&row).Error; err != nil {
		return "", credential.ErrVerificationFailed
	}
	a, err := currentAccount(ctx, row.UserUUID, synced)
	if err != nil {
		return "", err
	}
	var verified *webauthn.Credential
	if len(c.session.UserID) == 0 {
		_, verified, err = w.ValidatePasskeyLogin(func(rawID, userHandle []byte) (webauthn.User, error) {
			if !bytes.Equal(userHandle, a.WebAuthnID()) || row.CredentialID != base64.RawURLEncoding.EncodeToString(rawID) {
				return nil, credential.ErrVerificationFailed
			}
			return a, nil
		}, *c.session, parsed)
	} else {
		if !bytes.Equal(c.session.UserID, a.WebAuthnID()) {
			return "", credential.ErrVerificationFailed
		}
		verified, err = w.ValidateLogin(a, *c.session, parsed)
	}
	if err != nil || verified == nil || verified.Authenticator.CloneWarning || !verified.Flags.UserVerified {
		return "", credential.ErrVerificationFailed
	}
	if err = claimCounter(ctx, a, *verified); err != nil {
		return "", err
	}
	return a.id, nil
}
func BeginRegistration(ctx context.Context, b credential.Binding, name string) (any, string, error) {
	w, origin, synced, err := nativeVerifier(ctx)
	if err != nil {
		return nil, "", err
	}
	a, err := currentAccount(ctx, b.UserID, true)
	if err != nil {
		return nil, "", err
	}
	var u models.User
	if err := dbcore.GetDBInstance().WithContext(ctx).Select("username").Where("uuid = ?", b.UserID).Take(&u).Error; err == nil {
		a.username = u.Username
	}
	if len(a.records) >= 64 {
		return nil, "", credential.ErrLimit
	}
	options, session, err := w.BeginRegistration(a, webauthn.WithResidentKeyRequirement(protocol.ResidentKeyRequirementRequired), webauthn.WithExclusions(webauthn.Credentials(a.records).CredentialDescriptors()))
	if err != nil {
		return nil, "", err
	}
	token, err := remember(ceremony{session: session, register: true, binding: b, name: name, origin: origin, synced: synced, source: b.UserID}, "")
	return options, token, err
}
func CompleteRegistration(ctx context.Context, b credential.Binding, user models.User, token string, raw []byte) (models.UserPasskey, error) {
	var row models.UserPasskey
	c, err := take(token, true)
	if err != nil {
		return row, err
	}
	w, origin, synced, err := nativeVerifier(ctx)
	if err != nil || b != c.binding || origin != c.origin || synced != c.synced || w.Config.RPID != c.session.RelyingPartyID {
		return row, credential.ErrVerificationFailed
	}
	parsed, err := protocol.ParseCredentialCreationResponseBytes(raw)
	if err != nil {
		return row, credential.ErrVerificationFailed
	}
	key, err := w.CreateCredential(account{id: b.UserID, username: user.Username}, *c.session, parsed)
	if err != nil || !key.Flags.UserVerified || (!synced && key.Flags.BackupEligible) {
		return row, credential.ErrVerificationFailed
	}
	typ := "singleDevice"
	if key.Flags.BackupEligible {
		typ = "multiDevice"
	}
	row = models.UserPasskey{CredentialHash: idDigest(key.ID), CredentialID: base64.RawURLEncoding.EncodeToString(key.ID), UserUUID: b.UserID, RPID: w.Config.RPID, Name: c.name, PublicKey: base64.RawURLEncoding.EncodeToString(key.PublicKey), DeviceType: typ, BackedUp: key.Flags.BackupState}
	for _, v := range key.Transport {
		row.Transports = append(row.Transports, string(v))
	}
	err = dbcore.GetDBInstance().WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		if err := accounts.RotateCredentialVersion(tx, user); err != nil {
			return err
		}
		var revoked int64
		if err := tx.Model(&models.PasskeyRevocation{}).Where("credential_hash = ?", row.CredentialHash).Count(&revoked).Error; err != nil {
			return err
		}
		if revoked > 0 {
			return credential.ErrVerificationFailed
		}
		var enrolled int64
		if err := tx.Model(&models.UserPasskey{}).Where("user_uuid = ? AND rp_id = ?", user.UUID, w.Config.RPID).Count(&enrolled).Error; err != nil {
			return err
		}
		if enrolled >= 64 {
			return credential.ErrLimit
		}
		if err := tx.Omit("User").Create(&row).Error; err != nil {
			return err
		}
		return tx.Omit("User").Create(&models.PasskeyCounter{UserUUID: b.UserID, KeyDigest: keyDigest(key.ID, key.PublicKey), Counter: key.Authenticator.SignCount}).Error
	})
	if err == nil {
		InvalidateAccountFirstEnvelopeCache()
	}
	return row, err
}
func List(ctx context.Context, user string) ([]models.UserPasskey, error) {
	w, _, _, err := nativeVerifier(ctx)
	if err != nil {
		return nil, err
	}
	var rows []models.UserPasskey
	err = dbcore.GetDBInstance().WithContext(ctx).Select("credential_id", "name", "created_at", "last_used_at").Where("user_uuid = ? AND rp_id = ?", user, w.Config.RPID).Order("created_at, credential_hash").Find(&rows).Error
	return rows, err
}
func Delete(ctx context.Context, u models.User, id string, hasContact bool) error {
	w, _, synced, err := nativeVerifier(ctx)
	if err != nil {
		return err
	}
	k, err := credentialKey(id)
	if err != nil {
		return credential.ErrInvalidArgument
	}
	disablePasswordLogin, _ := config.GetAs[bool](config.DisablePasswordLoginKey, false)
	oauthEnabled, _ := config.GetAs[bool](config.OAuthEnabledKey, false)
	hasAlternateSignIn := !disablePasswordLogin || (oauthEnabled && strings.TrimSpace(u.SSOID) != "")
	err = dbcore.GetDBInstance().WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		user := u.UUID
		if err := accounts.RotateCredentialVersion(tx, u); err != nil {
			return err
		}
		var n int64
		q := tx.Model(&models.UserPasskey{}).Where("user_uuid = ? AND rp_id = ? AND credential_hash <> ?", user, w.Config.RPID, k)
		if !synced {
			q = q.Where("device_type = ?", "singleDevice")
		}
		if err := q.Count(&n).Error; err != nil {
			return err
		}
		if n == 0 && ((u.TwoFactor == "" && !hasContact) || !hasAlternateSignIn) {
			return accounts.ErrLastCredential
		}
		res := tx.Where("user_uuid = ? AND rp_id = ? AND credential_hash = ?", user, w.Config.RPID, k).Delete(&models.UserPasskey{})
		if res.Error != nil {
			return res.Error
		}
		if res.RowsAffected != 1 {
			return credential.ErrNotFound
		}
		revocation := models.PasskeyRevocation{
			CredentialHash: k,
			UserUUID:       user,
			RPID:           w.Config.RPID,
			RevokedAt:      time.Now().UTC(),
		}
		return tx.Clauses(clause.OnConflict{DoNothing: true}).Create(&revocation).Error
	})
	if err == nil {
		InvalidateAccountFirstEnvelopeCache()
	}
	return err
}
