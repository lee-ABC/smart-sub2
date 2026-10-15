package middleware

import (
	"bytes"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
)

func TestBPSLoginAuditNeverStoresAuthenticationInput(t *testing.T) {
	gin.SetMode(gin.TestMode)
	repo := &auditCaptureRepository{}
	audit := service.NewAuditLogService(repo, nil)
	audit.Start()
	r := gin.New()
	r.Use(func(c *gin.Context) { c.Set(string(ContextKeyUser), AuthSubject{UserID: 77}); c.Next() })
	r.Use(gin.HandlerFunc(NewAuditLogMiddleware(audit)))
	paths := []string{"/api/v1/admin/accounts/bps-login", "/api/v1/admin/accounts/bps-login/:jobID/input", "/api/v1/admin/accounts/bps-login/:jobID/import"}
	for _, p := range paths {
		r.POST(p, func(c *gin.Context) {
			var body map[string]any
			require.NoError(t, c.ShouldBindJSON(&body))
			require.NotEmpty(t, body)
			c.JSON(200, gin.H{"ok": true})
		})
	}
	for _, p := range []string{"/api/v1/admin/accounts/bps-login", "/api/v1/admin/accounts/bps-login/synthetic-job/input", "/api/v1/admin/accounts/bps-login/synthetic-job/import"} {
		q := httptest.NewRequest(http.MethodPost, p, bytes.NewBufferString(`{"email":"synthetic-audit@example.invalid","password":"private-audit-password","totp_secret":"PRIVATEAUDITTOTP","kind":"email_otp","value":"123456","content":"embedded-audit-token"}`))
		q.Header.Set("Content-Type", "application/json")
		w := httptest.NewRecorder()
		r.ServeHTTP(w, q)
		require.Equal(t, 200, w.Code)
	}
	audit.Stop()
	repo.mu.Lock()
	defer repo.mu.Unlock()
	require.Len(t, repo.logs, 3)
	for _, entry := range repo.logs {
		require.Equal(t, "<credential-bearing body omitted>", entry.RequestBody)
		require.NotContains(t, entry.RequestBody, "audit")
		require.NotContains(t, entry.RequestBody, "123456")
		require.EqualValues(t, 77, *entry.ActorUserID)
	}
}
