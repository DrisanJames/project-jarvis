package api

// DiscountBlogHTMLTemplate is the brand-accurate email skeleton for Discount Blog.
const DiscountBlogHTMLTemplate = `<!DOCTYPE html>
<html lang="en">
<head><meta charset="utf-8"><meta name="viewport" content="width=device-width, initial-scale=1.0"><title>{{SUBJECT}}</title></head>
<body style="margin:0;padding:0;background-color:#F3F4F6;font-family:-apple-system,BlinkMacSystemFont,'Segoe UI',Arial,sans-serif;">
<!-- preview text -->
<div style="display:none;max-height:0;overflow:hidden;">{{PREVIEW_TEXT}}</div>

<table width="100%" cellpadding="0" cellspacing="0" border="0" style="background-color:#F3F4F6;">
<tr><td align="center" style="padding:24px 16px;">

<!-- EMAIL CONTAINER -->
<table width="600" cellpadding="0" cellspacing="0" border="0" style="background-color:#FFFFFF;border-radius:12px;overflow:hidden;">

<!-- HEADER -->
<tr><td style="padding:28px 32px 20px 32px;border-bottom:1px solid #E5E7EB;">
  <span style="font-family:Georgia,'Times New Roman',serif;font-size:26px;font-weight:700;"><span style="color:#FF7B7B;">discount</span><span style="color:#5FCCB8;">blog</span></span>
</td></tr>

<!-- GREETING + INTRO -->
<tr><td style="padding:28px 32px 0 32px;">
  <p style="margin:0 0 16px 0;font-size:15px;line-height:1.6;color:#4B5563;">{{INTRO}}</p>
</td></tr>

<!-- BLOCK:ARTICLE_1 -->
<tr><td style="padding:12px 32px;">
  <table width="100%" cellpadding="0" cellspacing="0" border="0" style="border:1px solid #E5E7EB;border-top:3px solid #FF7B7B;border-radius:8px;overflow:hidden;">
  <tr><td style="padding:20px;">
    <span style="display:inline-block;background-color:#FEE2E2;color:#EF4444;font-size:11px;font-weight:700;text-transform:uppercase;letter-spacing:0.5px;padding:3px 10px;border-radius:4px;">{{ARTICLE_1_CATEGORY}}</span>
    <h2 style="margin:10px 0 8px 0;font-family:Georgia,'Times New Roman',serif;font-size:20px;font-weight:700;color:#1F2937;line-height:1.3;">{{ARTICLE_1_HEADLINE}}</h2>
    <p style="margin:0 0 14px 0;font-size:15px;line-height:1.6;color:#6B7280;">{{ARTICLE_1_SUMMARY}}</p>
    <a href="{{ARTICLE_1_URL}}" style="color:#FF7B7B;font-size:14px;font-weight:600;text-decoration:none;">{{ARTICLE_1_CTA}} &rarr;</a>
  </td></tr>
  </table>
</td></tr>
<!-- /BLOCK:ARTICLE_1 -->

<!-- BLOCK:ARTICLE_2 -->
<tr><td style="padding:12px 32px;">
  <table width="100%" cellpadding="0" cellspacing="0" border="0" style="border:1px solid #E5E7EB;border-radius:8px;overflow:hidden;">
  <tr><td style="padding:20px;">
    <span style="display:inline-block;background-color:#DBEAFE;color:#3B82F6;font-size:11px;font-weight:700;text-transform:uppercase;letter-spacing:0.5px;padding:3px 10px;border-radius:4px;">{{ARTICLE_2_CATEGORY}}</span>
    <h2 style="margin:10px 0 8px 0;font-family:Georgia,'Times New Roman',serif;font-size:20px;font-weight:700;color:#1F2937;line-height:1.3;">{{ARTICLE_2_HEADLINE}}</h2>
    <p style="margin:0 0 14px 0;font-size:15px;line-height:1.6;color:#6B7280;">{{ARTICLE_2_SUMMARY}}</p>
    <a href="{{ARTICLE_2_URL}}" style="color:#5FCCB8;font-size:14px;font-weight:600;text-decoration:none;">{{ARTICLE_2_CTA}} &rarr;</a>
  </td></tr>
  </table>
</td></tr>
<!-- /BLOCK:ARTICLE_2 -->

<!-- BLOCK:ARTICLE_3 -->
<tr><td style="padding:12px 32px;">
  <table width="100%" cellpadding="0" cellspacing="0" border="0" style="border:1px solid #E5E7EB;border-radius:8px;overflow:hidden;">
  <tr><td style="padding:20px;">
    <span style="display:inline-block;background-color:#FEF3C7;color:#D97706;font-size:11px;font-weight:700;text-transform:uppercase;letter-spacing:0.5px;padding:3px 10px;border-radius:4px;">{{ARTICLE_3_CATEGORY}}</span>
    <h2 style="margin:10px 0 8px 0;font-family:Georgia,'Times New Roman',serif;font-size:20px;font-weight:700;color:#1F2937;line-height:1.3;">{{ARTICLE_3_HEADLINE}}</h2>
    <p style="margin:0 0 14px 0;font-size:15px;line-height:1.6;color:#6B7280;">{{ARTICLE_3_SUMMARY}}</p>
    <a href="{{ARTICLE_3_URL}}" style="color:#FF7B7B;font-size:14px;font-weight:600;text-decoration:none;">{{ARTICLE_3_CTA}} &rarr;</a>
  </td></tr>
  </table>
</td></tr>
<!-- /BLOCK:ARTICLE_3 -->

<!-- CLOSING -->
<tr><td style="padding:16px 32px 28px 32px;">
  <p style="margin:0;font-size:15px;line-height:1.6;color:#4B5563;">{{CLOSING_LINE}}</p>
</td></tr>

<!-- FOOTER -->
<tr><td style="background-color:#FAFAFA;padding:24px 32px;border-top:1px solid #E5E7EB;">
  <p style="margin:0 0 8px 0;font-family:Georgia,'Times New Roman',serif;font-size:18px;font-weight:700;"><span style="color:#FF7B7B;">discount</span><span style="color:#5FCCB8;">blog</span></p>
  <p style="margin:0 0 16px 0;font-size:13px;color:#9CA3AF;line-height:1.5;">Smart savings for busy families. Real deals from real stores.</p>
  <p style="margin:0;font-size:12px;color:#9CA3AF;">
    You received this at {{ email }}.<br>
    <a href="{{ system.unsubscribe_url }}" style="color:#9CA3AF;text-decoration:underline;">Unsubscribe</a> &nbsp;|&nbsp;
    <a href="https://discountblog.com" style="color:#9CA3AF;text-decoration:underline;">discountblog.com</a>
  </p>
</td></tr>

</table>
<!-- /EMAIL CONTAINER -->

</td></tr>
</table>
</body>
</html>`

// QuizFiestaHTMLTemplate is the brand-accurate email skeleton for QuizFiesta.
const QuizFiestaHTMLTemplate = `<!DOCTYPE html>
<html lang="en">
<head><meta charset="utf-8"><meta name="viewport" content="width=device-width, initial-scale=1.0"><title>{{SUBJECT}}</title></head>
<body style="margin:0;padding:0;background-color:#000000;font-family:'Courier New',Courier,monospace;">
<!-- preview text -->
<div style="display:none;max-height:0;overflow:hidden;">{{PREVIEW_TEXT}}</div>

<table width="100%" cellpadding="0" cellspacing="0" border="0" style="background-color:#000000;">
<tr><td align="center" style="padding:0;">

<!-- EMAIL CONTAINER -->
<table width="600" cellpadding="0" cellspacing="0" border="0" style="background-color:#0A0014;">

<!-- HEADER -->
<tr><td style="padding:24px 28px 16px 28px;border-bottom:2px solid #8B5CF6;">
  <table width="100%" cellpadding="0" cellspacing="0" border="0">
  <tr>
    <td style="font-family:'Courier New',Courier,monospace;font-size:22px;font-weight:700;color:#FFFFFF;letter-spacing:4px;">QUIZ FIESTA</td>
    <td align="right" style="font-size:20px;">&#x1F47E; &#x1F47E; &#x1F47E;</td>
  </tr>
  </table>
</td></tr>

<!-- INTRO -->
<tr><td style="padding:24px 28px 8px 28px;">
  <p style="margin:0;font-family:'Courier New',Courier,monospace;font-size:14px;line-height:1.6;color:#A0A0B0;">{{INTRO}}</p>
</td></tr>

<!-- DIVIDER -->
<tr><td style="padding:8px 28px;"><table width="100%" cellpadding="0" cellspacing="0" border="0"><tr><td style="border-bottom:1px solid #8B5CF6;box-shadow:0 1px 4px rgba(139,92,246,0.3);font-size:1px;height:1px;">&nbsp;</td></tr></table></td></tr>

<!-- BLOCK:ARTICLE_1 -->
<tr><td style="padding:16px 28px;">
  <table width="100%" cellpadding="0" cellspacing="0" border="0" style="background-color:#1A0A2E;border:2px solid #8B5CF6;">
  <tr><td style="padding:20px;">
    <p style="margin:0 0 6px 0;font-size:28px;">{{ARTICLE_1_EMOJI}}</p>
    <h2 style="margin:0 0 10px 0;font-family:'Courier New',Courier,monospace;font-size:18px;font-weight:700;color:#FFFFFF;text-transform:uppercase;letter-spacing:2px;">{{ARTICLE_1_HEADLINE}}</h2>
    <p style="margin:0 0 16px 0;font-family:'Courier New',Courier,monospace;font-size:13px;line-height:1.5;color:#A0A0B0;">{{ARTICLE_1_SUMMARY}}</p>
    <a href="{{ARTICLE_1_URL}}" style="display:inline-block;background-color:#8B5CF6;color:#FFFFFF;font-family:'Courier New',Courier,monospace;font-size:13px;font-weight:700;text-transform:uppercase;letter-spacing:3px;text-decoration:none;padding:12px 24px;border:2px solid #39FF14;">{{ARTICLE_1_CTA}}</a>
  </td></tr>
  </table>
</td></tr>
<!-- /BLOCK:ARTICLE_1 -->

<!-- BLOCK:ARTICLE_2 -->
<tr><td style="padding:8px 28px;">
  <table width="100%" cellpadding="0" cellspacing="0" border="0" style="background-color:#1A0A2E;border:2px solid #8B5CF6;">
  <tr><td style="padding:20px;">
    <p style="margin:0 0 6px 0;font-size:28px;">{{ARTICLE_2_EMOJI}}</p>
    <h2 style="margin:0 0 10px 0;font-family:'Courier New',Courier,monospace;font-size:18px;font-weight:700;color:#FFFFFF;text-transform:uppercase;letter-spacing:2px;">{{ARTICLE_2_HEADLINE}}</h2>
    <p style="margin:0 0 16px 0;font-family:'Courier New',Courier,monospace;font-size:13px;line-height:1.5;color:#A0A0B0;">{{ARTICLE_2_SUMMARY}}</p>
    <a href="{{ARTICLE_2_URL}}" style="display:inline-block;background-color:transparent;color:#39FF14;font-family:'Courier New',Courier,monospace;font-size:13px;font-weight:700;text-transform:uppercase;letter-spacing:3px;text-decoration:none;padding:12px 24px;border:2px solid #8B5CF6;">{{ARTICLE_2_CTA}}</a>
  </td></tr>
  </table>
</td></tr>
<!-- /BLOCK:ARTICLE_2 -->

<!-- BLOCK:ARTICLE_3 -->
<tr><td style="padding:8px 28px;">
  <table width="100%" cellpadding="0" cellspacing="0" border="0" style="background-color:#1A0A2E;border:2px solid #8B5CF6;">
  <tr><td style="padding:20px;">
    <p style="margin:0 0 6px 0;font-size:28px;">{{ARTICLE_3_EMOJI}}</p>
    <h2 style="margin:0 0 10px 0;font-family:'Courier New',Courier,monospace;font-size:18px;font-weight:700;color:#FFFFFF;text-transform:uppercase;letter-spacing:2px;">{{ARTICLE_3_HEADLINE}}</h2>
    <p style="margin:0 0 16px 0;font-family:'Courier New',Courier,monospace;font-size:13px;line-height:1.5;color:#A0A0B0;">{{ARTICLE_3_SUMMARY}}</p>
    <a href="{{ARTICLE_3_URL}}" style="display:inline-block;background-color:#8B5CF6;color:#FFFFFF;font-family:'Courier New',Courier,monospace;font-size:13px;font-weight:700;text-transform:uppercase;letter-spacing:3px;text-decoration:none;padding:12px 24px;border:2px solid #39FF14;">{{ARTICLE_3_CTA}}</a>
  </td></tr>
  </table>
</td></tr>
<!-- /BLOCK:ARTICLE_3 -->

<!-- CLOSING -->
<tr><td style="padding:16px 28px 8px 28px;">
  <p style="margin:0;font-family:'Courier New',Courier,monospace;font-size:14px;line-height:1.6;color:#A0A0B0;">{{CLOSING_LINE}}</p>
</td></tr>

<!-- INSERT COIN DIVIDER -->
<tr><td align="center" style="padding:16px 28px;">
  <p style="margin:0;font-family:'Courier New',Courier,monospace;font-size:14px;color:#FFD700;letter-spacing:3px;">&#9724; INSERT COIN TO CONTINUE &#9724;</p>
</td></tr>

<!-- FOOTER -->
<tr><td style="padding:16px 28px 24px 28px;border-top:2px solid #8B5CF6;">
  <p style="margin:0 0 8px 0;font-family:'Courier New',Courier,monospace;font-size:12px;color:#666666;">
    <span style="color:#39FF14;">&gt;</span> &copy; 2026 PLAYER_ONE // ALL RIGHTS RESERVED
  </p>
  <p style="margin:0;font-family:'Courier New',Courier,monospace;font-size:11px;color:#555555;">
    Sent to {{ email }} |
    <a href="{{ system.unsubscribe_url }}" style="color:#8B5CF6;text-decoration:none;">QUIT GAME</a> |
    <a href="https://quizfiesta.com" style="color:#8B5CF6;text-decoration:none;">QUIZFIESTA.COM</a>
  </p>
</td></tr>

</table>
<!-- /EMAIL CONTAINER -->

</td></tr>
</table>
</body>
</html>`
