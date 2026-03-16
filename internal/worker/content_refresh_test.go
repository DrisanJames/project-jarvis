package worker

import (
	"context"
	"database/sql"
	"testing"

	"github.com/DATA-DOG/go-sqlmock"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestGetUnusedWaveByType_ReturnsMatchingWave(t *testing.T) {
	db, mock, err := sqlmock.New()
	require.NoError(t, err)
	defer db.Close()

	editorialJSON := `{"wave_index":0,"subject":"AI Subject","intro":"Fresh intro"}`

	rows := sqlmock.NewRows([]string{
		"wave_index", "subject", "preview_text", "from_name", "html_content", "diagnostics", "editorial_json",
	}).AddRow(
		0, "AI Subject", "Preview", "Discount Blog", "<html>rendered</html>",
		sql.NullString{Valid: false}, sql.NullString{String: editorialJSON, Valid: true},
	)

	mock.ExpectQuery(`UPDATE mailing_wave_content_cache`).
		WithArgs("discountblog", "welcome").
		WillReturnRows(rows)

	cached, err := GetUnusedWaveByType(context.Background(), db, "discountblog", "welcome")
	require.NoError(t, err)
	require.NotNil(t, cached)

	assert.Equal(t, "AI Subject", cached.Variation.Subject)
	assert.Equal(t, "Preview", cached.Variation.PreviewText)
	assert.Equal(t, "<html>rendered</html>", cached.Variation.HTMLContent)
	assert.Equal(t, editorialJSON, string(cached.EditorialJSON))

	assert.NoError(t, mock.ExpectationsWereMet())
}

func TestGetUnusedWaveByType_EmptyType_MatchesAny(t *testing.T) {
	db, mock, err := sqlmock.New()
	require.NoError(t, err)
	defer db.Close()

	rows := sqlmock.NewRows([]string{
		"wave_index", "subject", "preview_text", "from_name", "html_content", "diagnostics", "editorial_json",
	}).AddRow(
		1, "Newsletter Sub", "", "Quiz Fiesta", "<html>newsletter</html>",
		sql.NullString{Valid: false}, sql.NullString{Valid: false},
	)

	mock.ExpectQuery(`UPDATE mailing_wave_content_cache`).
		WithArgs("quizfiesta", "").
		WillReturnRows(rows)

	cached, err := GetUnusedWaveByType(context.Background(), db, "quizfiesta", "")
	require.NoError(t, err)
	require.NotNil(t, cached)

	assert.Equal(t, "Newsletter Sub", cached.Variation.Subject)
	assert.Nil(t, cached.EditorialJSON, "editorial_json should be nil when column is NULL")

	assert.NoError(t, mock.ExpectationsWereMet())
}

func TestGetUnusedWaveByType_NoRows_ReturnsError(t *testing.T) {
	db, mock, err := sqlmock.New()
	require.NoError(t, err)
	defer db.Close()

	mock.ExpectQuery(`UPDATE mailing_wave_content_cache`).
		WithArgs("discountblog", "welcome").
		WillReturnError(sql.ErrNoRows)

	cached, err := GetUnusedWaveByType(context.Background(), db, "discountblog", "welcome")
	assert.Error(t, err)
	assert.Nil(t, cached)
	assert.Contains(t, err.Error(), "no cached content")

	assert.NoError(t, mock.ExpectationsWereMet())
}

func TestGetUnusedWave_DelegatesToByType(t *testing.T) {
	db, mock, err := sqlmock.New()
	require.NoError(t, err)
	defer db.Close()

	rows := sqlmock.NewRows([]string{
		"wave_index", "subject", "preview_text", "from_name", "html_content", "diagnostics", "editorial_json",
	}).AddRow(
		2, "Any Type Sub", "Preview", "Brand", "<html>any</html>",
		sql.NullString{Valid: false}, sql.NullString{Valid: false},
	)

	mock.ExpectQuery(`UPDATE mailing_wave_content_cache`).
		WithArgs("discountblog", "").
		WillReturnRows(rows)

	variation, err := GetUnusedWave(context.Background(), db, "discountblog")
	require.NoError(t, err)
	require.NotNil(t, variation)

	assert.Equal(t, "Any Type Sub", variation.Subject)
	assert.Equal(t, 2, variation.WaveIndex)

	assert.NoError(t, mock.ExpectationsWereMet())
}
