// +build ignore

package main

import (
	"context"
	"database/sql"
	"fmt"
	"log"
	"os"
	"time"

	"github.com/google/uuid"
	_ "github.com/lib/pq"
)

// This Day In History - Welcome Quiz Template HTML
const welcomeQuizHTML = `<!DOCTYPE html>
<html lang="en">
<head>
    <meta charset="UTF-8">
    <meta name="viewport" content="width=device-width, initial-scale=1.0">
    <title>Today's History Quiz</title>
    <style>
        body {
            font-family: 'Georgia', serif;
            background-color: #f5f5dc;
            margin: 0;
            padding: 0;
            color: #333;
        }
        .container {
            max-width: 600px;
            margin: 0 auto;
            background: white;
            border-radius: 8px;
            overflow: hidden;
            box-shadow: 0 4px 6px rgba(0,0,0,0.1);
        }
        .header {
            background: linear-gradient(135deg, #2c3e50 0%, #34495e 100%);
            color: white;
            padding: 30px 20px;
            text-align: center;
        }
        .header h1 {
            margin: 0;
            font-size: 28px;
            font-weight: normal;
        }
        .header .date {
            font-size: 14px;
            opacity: 0.8;
            margin-top: 10px;
        }
        .quiz-intro {
            padding: 25px;
            background: #f9f9f9;
            text-align: center;
            border-bottom: 1px solid #eee;
        }
        .quiz-intro h2 {
            color: #2c3e50;
            margin: 0 0 10px 0;
            font-size: 22px;
        }
        .quiz-intro p {
            color: #666;
            margin: 0;
            font-size: 15px;
        }
        .question-card {
            padding: 25px;
            border-bottom: 1px solid #eee;
        }
        .question-number {
            background: #e74c3c;
            color: white;
            padding: 5px 12px;
            border-radius: 20px;
            font-size: 12px;
            font-weight: bold;
            display: inline-block;
            margin-bottom: 15px;
        }
        .question-number.q2 {
            background: #3498db;
        }
        .question-number.q3 {
            background: #27ae60;
        }
        .question-text {
            font-size: 18px;
            color: #2c3e50;
            margin-bottom: 20px;
            line-height: 1.5;
        }
        .answer-options {
            display: block;
        }
        .answer-btn {
            display: block;
            width: 100%;
            padding: 12px 15px;
            margin: 8px 0;
            background: #fff;
            border: 2px solid #ddd;
            border-radius: 6px;
            text-align: left;
            font-size: 15px;
            color: #333;
            text-decoration: none;
            transition: all 0.2s;
        }
        .answer-btn:hover {
            border-color: #3498db;
            background: #f0f8ff;
        }
        .fun-fact {
            background: #fff9e6;
            padding: 20px 25px;
            border-left: 4px solid #f1c40f;
            margin: 0;
        }
        .fun-fact h3 {
            color: #d4a41e;
            margin: 0 0 10px 0;
            font-size: 14px;
            text-transform: uppercase;
            letter-spacing: 1px;
        }
        .fun-fact p {
            color: #666;
            margin: 0;
            font-size: 14px;
            line-height: 1.6;
        }
        .cta-section {
            padding: 30px;
            text-align: center;
            background: linear-gradient(135deg, #3498db 0%, #2980b9 100%);
        }
        .cta-section h3 {
            color: white;
            margin: 0 0 15px 0;
            font-size: 20px;
        }
        .cta-btn {
            display: inline-block;
            background: white;
            color: #3498db;
            padding: 15px 40px;
            text-decoration: none;
            border-radius: 30px;
            font-weight: bold;
            font-size: 16px;
        }
        .footer {
            background: #2c3e50;
            color: #bbb;
            padding: 20px;
            text-align: center;
            font-size: 12px;
        }
        .footer a {
            color: #3498db;
            text-decoration: none;
        }
        .social-links {
            margin: 15px 0;
        }
        .social-links a {
            display: inline-block;
            margin: 0 10px;
            color: white;
            text-decoration: none;
        }
    </style>
</head>
<body>
    <div class="container">
        <div class="header">
            <h1>📚 This Day In History</h1>
            <div class="date">{{.Date}} • Daily Quiz Edition</div>
        </div>
        
        <div class="quiz-intro">
            <h2>🧠 Today's Triple Challenge</h2>
            <p>Test your knowledge with three fascinating questions from history!</p>
        </div>
        
        <!-- Question 1: Entertainment -->
        <div class="question-card">
            <span class="question-number">QUESTION 1</span>
            <div class="question-text">
                <strong>🎬 Entertainment:</strong> On April 10, 1912, a famous "unsinkable" ship departed on its maiden voyage. Which ship was it?
            </div>
            <div class="answer-options">
                <a href="{{.AnswerURL}}?q=1&a=lusitania" class="answer-btn">A) RMS Lusitania</a>
                <a href="{{.AnswerURL}}?q=1&a=titanic" class="answer-btn">B) RMS Titanic</a>
                <a href="{{.AnswerURL}}?q=1&a=olympic" class="answer-btn">C) RMS Olympic</a>
                <a href="{{.AnswerURL}}?q=1&a=britannic" class="answer-btn">D) HMHS Britannic</a>
            </div>
        </div>
        
        <!-- Question 2: Aviation -->
        <div class="question-card">
            <span class="question-number q2">QUESTION 2</span>
            <div class="question-text">
                <strong>✈️ Aviation:</strong> The Wright Brothers made their historic first flight at Kitty Hawk in which year?
            </div>
            <div class="answer-options">
                <a href="{{.AnswerURL}}?q=2&a=1901" class="answer-btn">A) 1901</a>
                <a href="{{.AnswerURL}}?q=2&a=1903" class="answer-btn">B) 1903</a>
                <a href="{{.AnswerURL}}?q=2&a=1905" class="answer-btn">C) 1905</a>
                <a href="{{.AnswerURL}}?q=2&a=1907" class="answer-btn">D) 1907</a>
            </div>
        </div>
        
        <!-- Question 3: Sports -->
        <div class="question-card">
            <span class="question-number q3">QUESTION 3</span>
            <div class="question-text">
                <strong>⚾ Sports:</strong> Who holds the record for most career home runs in Major League Baseball?
            </div>
            <div class="answer-options">
                <a href="{{.AnswerURL}}?q=3&a=ruth" class="answer-btn">A) Babe Ruth</a>
                <a href="{{.AnswerURL}}?q=3&a=aaron" class="answer-btn">B) Hank Aaron</a>
                <a href="{{.AnswerURL}}?q=3&a=bonds" class="answer-btn">C) Barry Bonds</a>
                <a href="{{.AnswerURL}}?q=3&a=mays" class="answer-btn">D) Willie Mays</a>
            </div>
        </div>
        
        <div class="fun-fact">
            <h3>💡 Did You Know?</h3>
            <p>The Titanic's band famously continued playing as the ship sank. While "Nearer, My God, to Thee" is often cited as their final song, some survivors reported they played the waltz "Autumn".</p>
        </div>
        
        <div class="cta-section">
            <h3>Ready to See Your Score?</h3>
            <a href="{{.ResultsURL}}" class="cta-btn">View My Results →</a>
        </div>
        
        <div class="footer">
            <p>You're receiving this because you subscribed to This Day In History.</p>
            <p>
                <a href="{{.PreferencesURL}}">Manage Preferences</a> • 
                <a href="{{.UnsubscribeURL}}">Unsubscribe</a>
            </p>
            <p>© 2026 This Day In History. All rights reserved.</p>
        </div>
    </div>
</body>
</html>`

func main() {
	// Get database connection string from environment
	dbURL := os.Getenv("DATABASE_URL")
	if dbURL == "" {
		dbURL = "postgres://mailing:mailing@localhost:5432/mailing?sslmode=disable"
	}

	db, err := sql.Open("postgres", dbURL)
	if err != nil {
		log.Fatalf("Failed to connect to database: %v", err)
	}
	defer db.Close()

	ctx := context.Background()

	// Organization ID from environment or query for default
	orgIDStr := os.Getenv("ORG_ID")
	var orgID uuid.UUID
	if orgIDStr != "" {
		var err error
		orgID, err = uuid.Parse(orgIDStr)
		if err != nil {
			log.Fatalf("Invalid ORG_ID: %v", err)
		}
	} else {
		// Query for default organization
		err = db.QueryRowContext(ctx, `
			SELECT id FROM organizations WHERE is_default = true OR slug = 'default' LIMIT 1
		`).Scan(&orgID)
		if err != nil {
			log.Printf("No default organization found, will create one")
			orgID = uuid.New()
		}
	}

	// Ensure organization exists
	_, err = db.ExecContext(ctx, `
		INSERT INTO organizations (id, name, slug, status, created_at, updated_at)
		VALUES ($1, 'Default Organization', 'default', 'active', NOW(), NOW())
		ON CONFLICT (id) DO NOTHING
	`, orgID)
	if err != nil {
		log.Printf("Warning: Could not ensure organization exists: %v", err)
	}

	fmt.Println("🚀 Seeding This Day In History Template System...")

	// Step 1: Create template folders
	fmt.Println("\n📁 Creating template folders...")

	// Create parent folder "This Day In History"
	parentFolderID := uuid.New()
	_, err = db.ExecContext(ctx, `
		INSERT INTO mailing_template_folders (id, organization_id, parent_id, name, created_at, updated_at)
		VALUES ($1, $2, NULL, 'This Day In History', NOW(), NOW())
		ON CONFLICT (organization_id, parent_id, name) DO UPDATE SET updated_at = NOW()
		RETURNING id
	`, parentFolderID, orgID)
	if err != nil {
		// Try to get existing
		err = db.QueryRowContext(ctx, `
			SELECT id FROM mailing_template_folders 
			WHERE organization_id = $1 AND name = 'This Day In History' AND parent_id IS NULL
		`, orgID).Scan(&parentFolderID)
		if err != nil {
			log.Fatalf("Failed to create/get parent folder: %v", err)
		}
	}
	fmt.Printf("   ✓ Created folder: This Day In History (ID: %s)\n", parentFolderID)

	// Create subfolder "Welcome Series"
	welcomeFolderID := uuid.New()
	_, err = db.ExecContext(ctx, `
		INSERT INTO mailing_template_folders (id, organization_id, parent_id, name, created_at, updated_at)
		VALUES ($1, $2, $3, 'Welcome Series', NOW(), NOW())
		ON CONFLICT DO NOTHING
	`, welcomeFolderID, orgID, parentFolderID)
	if err != nil {
		err = db.QueryRowContext(ctx, `
			SELECT id FROM mailing_template_folders 
			WHERE organization_id = $1 AND name = 'Welcome Series' AND parent_id = $2
		`, orgID, parentFolderID).Scan(&welcomeFolderID)
		if err != nil {
			log.Fatalf("Failed to create/get Welcome Series folder: %v", err)
		}
	}
	fmt.Printf("   ✓ Created folder: Welcome Series (ID: %s)\n", welcomeFolderID)

	// Step 2: Create the Welcome Quiz template
	fmt.Println("\n📧 Creating Welcome Quiz template...")

	templateID := uuid.New()
	_, err = db.ExecContext(ctx, `
		INSERT INTO mailing_templates (
			id, organization_id, folder_id, name, description, subject, 
			from_name, from_email, html_content, preview_text, status, created_at, updated_at
		) VALUES (
			$1, $2, $3, 'Welcome Quiz', 
			'Daily history quiz template with 3 questions covering Entertainment, Aviation, and Sports',
			'🧠 Can You Go 3 for 3? Today''s Triple Challenge',
			'This Day In History', 'quiz@thisdayinhistory.com',
			$4,
			'From Titanic to the Wright Brothers - How much do you really know?',
			'active', NOW(), NOW()
		)
		ON CONFLICT DO NOTHING
	`, templateID, orgID, welcomeFolderID, welcomeQuizHTML)
	if err != nil {
		log.Printf("Warning creating template: %v", err)
		// Try to get existing template
		err = db.QueryRowContext(ctx, `
			SELECT id FROM mailing_templates 
			WHERE organization_id = $1 AND folder_id = $2 AND name = 'Welcome Quiz'
		`, orgID, welcomeFolderID).Scan(&templateID)
		if err != nil {
			log.Fatalf("Failed to create/get template: %v", err)
		}
	}
	fmt.Printf("   ✓ Created template: Welcome Quiz (ID: %s)\n", templateID)

	// Step 3: Create an A/B test for the Welcome Quiz
	fmt.Println("\n🧪 Creating A/B Test with subject line variants...")

	testID := uuid.New()
	_, err = db.ExecContext(ctx, `
		INSERT INTO mailing_ab_tests (
			id, organization_id, name, description, test_type,
			split_type, test_sample_percent,
			winner_metric, winner_wait_hours, winner_auto_select,
			winner_confidence_threshold, winner_min_sample_size,
			status, created_at, updated_at
		) VALUES (
			$1, $2, 
			'Welcome Quiz Subject Line Test',
			'Testing 4 different subject lines for the Welcome Quiz to maximize open rates',
			'subject_line',
			'percentage', 100,
			'open_rate', 4, TRUE,
			0.95, 100,
			'draft', NOW(), NOW()
		)
		ON CONFLICT DO NOTHING
	`, testID, orgID)
	if err != nil {
		log.Printf("Warning creating A/B test: %v", err)
	}
	fmt.Printf("   ✓ Created A/B Test (ID: %s)\n", testID)

	// Step 4: Create A/B variants with dynamic subject lines and preheaders
	fmt.Println("\n📊 Creating A/B Test variants...")

	variants := []struct {
		Name      string
		Label     string
		Subject   string
		Preheader string
		IsControl bool
	}{
		{
			Name:      "A",
			Label:     "Emoji Challenge",
			Subject:   "🧠 Can You Go 3 for 3? Today's Triple Challenge",
			Preheader: "From Titanic to the Wright Brothers - How much do you really know?",
			IsControl: true,
		},
		{
			Name:      "B",
			Label:     "Excitement",
			Subject:   "Your Daily History Quiz is Ready! 🎉",
			Preheader: "Quick! The answers might surprise you...",
			IsControl: false,
		},
		{
			Name:      "C",
			Label:     "Curiosity",
			Subject:   "Did You Know? Test Your History Knowledge Today",
			Preheader: "Today's challenge: Entertainment, Aviation, and Sports",
			IsControl: false,
		},
		{
			Name:      "D",
			Label:     "Competition",
			Subject:   "3 Questions. 3 Chances. How Many Can You Get Right?",
			Preheader: "Join thousands testing their knowledge daily",
			IsControl: false,
		},
	}

	splitPercent := 25 // Equal split for 4 variants

	for _, v := range variants {
		variantID := uuid.New()
		_, err = db.ExecContext(ctx, `
			INSERT INTO mailing_ab_variants (
				id, test_id, variant_name, variant_label,
				subject, preheader, html_content,
				split_percent, is_control, created_at
			) VALUES (
				$1, $2, $3, $4,
				$5, $6, $7,
				$8, $9, NOW()
			)
			ON CONFLICT DO NOTHING
		`, variantID, testID, v.Name, v.Label, v.Subject, v.Preheader, welcomeQuizHTML, splitPercent, v.IsControl)
		if err != nil {
			log.Printf("Warning creating variant %s: %v", v.Name, err)
		} else {
			controlStr := ""
			if v.IsControl {
				controlStr = " (CONTROL)"
			}
			fmt.Printf("   ✓ Variant %s%s: \"%s\"\n", v.Name, controlStr, v.Subject)
			fmt.Printf("      Preheader: \"%s\"\n", v.Preheader)
		}
	}

	// Step 5: Create additional template folders for organization
	fmt.Println("\n📁 Creating additional folder structure...")

	additionalFolders := []struct {
		Name     string
		ParentID *uuid.UUID
	}{
		{"Daily Digest", nil},
		{"Special Events", nil},
		{"Re-engagement", nil},
	}

	for _, f := range additionalFolders {
		folderID := uuid.New()
		var parentID interface{} = nil
		if f.ParentID != nil {
			parentID = *f.ParentID
		}
		_, err = db.ExecContext(ctx, `
			INSERT INTO mailing_template_folders (id, organization_id, parent_id, name, created_at, updated_at)
			VALUES ($1, $2, $3, $4, NOW(), NOW())
			ON CONFLICT DO NOTHING
		`, folderID, orgID, parentID, f.Name)
		if err != nil {
			log.Printf("Warning creating folder %s: %v", f.Name, err)
		} else {
			fmt.Printf("   ✓ Created folder: %s\n", f.Name)
		}
	}

	// Summary
	fmt.Println("\n✅ Seed completed successfully!")
	fmt.Println("\n📋 Summary:")
	fmt.Println("   • Template Folders: This Day In History/Welcome Series")
	fmt.Println("   • Template: Welcome Quiz")
	fmt.Println("   • A/B Test: Welcome Quiz Subject Line Test")
	fmt.Println("   • Variants: 4 (A-D with different subject lines and preheaders)")
	fmt.Println("\n🔗 API Endpoints:")
	fmt.Println("   GET  /api/mailing/template-folders      - List all folders")
	fmt.Println("   GET  /api/mailing/template-folders/tree - Get folder tree structure")
	fmt.Println("   POST /api/mailing/template-folders      - Create folder")
	fmt.Println("   GET  /api/mailing/templates             - List templates")
	fmt.Println("   POST /api/mailing/templates             - Create template")
	fmt.Println("   GET  /api/mailing/ab-tests              - List A/B tests")
	fmt.Println("   GET  /api/mailing/ab-tests/{id}/variants - Get test variants")
	fmt.Printf("\n🔑 Organization ID: %s\n", orgID)
	fmt.Printf("🗂️  Template ID: %s\n", templateID)
	fmt.Printf("🧪 A/B Test ID: %s\n", testID)
	fmt.Printf("\n⏰ Completed at: %s\n", time.Now().Format(time.RFC3339))
}
