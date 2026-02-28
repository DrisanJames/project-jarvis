import React, { useState, useCallback, useEffect, useRef } from 'react';
import { useEditor, EditorContent } from '@tiptap/react';
import StarterKit from '@tiptap/starter-kit';
import Link from '@tiptap/extension-link';
import Image from '@tiptap/extension-image';
import TextAlign from '@tiptap/extension-text-align';
import Underline from '@tiptap/extension-underline';
import Placeholder from '@tiptap/extension-placeholder';
import { Color } from '@tiptap/extension-color';
import { TextStyle } from '@tiptap/extension-text-style';
import { FontAwesomeIcon } from '@fortawesome/react-fontawesome';
import {
  faBold,
  faItalic,
  faUnderline,
  faStrikethrough,
  faAlignLeft,
  faAlignCenter,
  faAlignRight,
  faListUl,
  faListOl,
  faLink,
  faUnlink,
  faImage,
  faQuoteRight,
  faUndo,
  faRedo,
  faDesktop,
  faMobileAlt,
  faExpand,
  faCompress,
  faEye,
  faFileCode,
  faMagic,
  faUpload,
  faSpinner,
  faCheckCircle,
  faExclamationTriangle,
} from '@fortawesome/free-solid-svg-icons';
import './EmailEditor.css';

// =============================================================================
// TYPES
// =============================================================================

interface EmailEditorProps {
  content: string;
  onChange: (content: string) => void;
  placeholder?: string;
  minHeight?: number;
}

type ViewMode = 'edit' | 'preview' | 'code';
type DevicePreview = 'desktop' | 'mobile';

// =============================================================================
// TOOLBAR BUTTON
// =============================================================================

interface ToolbarButtonProps {
  icon: any;
  onClick: () => void;
  isActive?: boolean;
  disabled?: boolean;
  title: string;
}

const ToolbarButton: React.FC<ToolbarButtonProps> = ({ 
  icon, onClick, isActive = false, disabled = false, title 
}) => (
  <button
    type="button"
    className={`ee-toolbar-btn ${isActive ? 'active' : ''}`}
    onClick={onClick}
    disabled={disabled}
    title={title}
  >
    <FontAwesomeIcon icon={icon} />
  </button>
);

// =============================================================================
// MAIN COMPONENT
// =============================================================================

export const EmailEditor: React.FC<EmailEditorProps> = ({
  content,
  onChange,
  placeholder = 'Start typing your email content...',
  minHeight = 400,
}) => {
  const [viewMode, setViewMode] = useState<ViewMode>('edit');
  const [devicePreview, setDevicePreview] = useState<DevicePreview>('desktop');
  const [isFullscreen, setIsFullscreen] = useState(false);
  const [showLinkModal, setShowLinkModal] = useState(false);
  const [linkUrl, setLinkUrl] = useState('');
  const [showImageModal, setShowImageModal] = useState(false);
  const [imageUrl, setImageUrl] = useState('');
  const [imageUploadMode, setImageUploadMode] = useState<'url' | 'upload'>('upload');
  const [uploading, setUploading] = useState(false);
  const [uploadError, setUploadError] = useState('');
  const [uploadSuccess, setUploadSuccess] = useState('');
  const fileInputRef = useRef<HTMLInputElement>(null);

  // Track whether the last content change came from inside the editor (typing)
  // vs. outside (e.g. creative selector setting HTML). This prevents infinite loops.
  const isInternalUpdate = useRef(false);

  const editor = useEditor({
    extensions: [
      StarterKit.configure({
        heading: {
          levels: [1, 2, 3],
        },
      }),
      Underline,
      TextStyle,
      Color,
      Link.configure({
        openOnClick: false,
        HTMLAttributes: {
          class: 'email-link',
        },
      }),
      Image.configure({
        HTMLAttributes: {
          class: 'email-image',
        },
      }),
      TextAlign.configure({
        types: ['heading', 'paragraph'],
      }),
      Placeholder.configure({
        placeholder,
      }),
    ],
    content,
    onUpdate: ({ editor }) => {
      isInternalUpdate.current = true;
      onChange(editor.getHTML());
    },
  });

  // Sync external content changes into the editor (e.g., from Everflow creative selector)
  useEffect(() => {
    if (!editor) return;
    // Skip if the change came from the editor itself (typing, formatting, etc.)
    if (isInternalUpdate.current) {
      isInternalUpdate.current = false;
      return;
    }
    // Only update if the content actually differs from what's in the editor
    const currentHTML = editor.getHTML();
    if (content !== currentHTML) {
      editor.commands.setContent(content, { emitUpdate: false });
    }
  }, [content, editor]);

  // Link handling
  const handleSetLink = useCallback(() => {
    if (!editor) return;
    
    if (linkUrl === '') {
      editor.chain().focus().extendMarkRange('link').unsetLink().run();
    } else {
      editor.chain().focus().extendMarkRange('link').setLink({ href: linkUrl }).run();
    }
    setShowLinkModal(false);
    setLinkUrl('');
  }, [editor, linkUrl]);

  const openLinkModal = () => {
    if (!editor) return;
    const previousUrl = editor.getAttributes('link').href;
    setLinkUrl(previousUrl || '');
    setShowLinkModal(true);
  };

  // Image handling - insert from URL
  const handleAddImage = useCallback(() => {
    if (!editor || !imageUrl) return;
    editor.chain().focus().setImage({ src: imageUrl }).run();
    setShowImageModal(false);
    setImageUrl('');
    setUploadError('');
    setUploadSuccess('');
  }, [editor, imageUrl]);

  // Image handling - upload file to CDN
  const handleFileUpload = useCallback(async (file: File) => {
    if (!editor) return;
    setUploading(true);
    setUploadError('');
    setUploadSuccess('');

    try {
      const formData = new FormData();
      formData.append('file', file);

      const res = await fetch('/api/mailing/images', {
        method: 'POST',
        body: formData,
        credentials: 'include',
      });

      if (!res.ok) {
        const errData = await res.json().catch(() => ({}));
        throw new Error(errData.error || `Upload failed (${res.status})`);
      }

      const data = await res.json();
      const cdnUrl = data.cdn_url || data.url;

      if (!cdnUrl) {
        throw new Error('No URL returned from upload');
      }

      // Insert into editor
      editor.chain().focus().setImage({ src: cdnUrl }).run();
      setUploadSuccess(`Uploaded: ${file.name}`);
      setTimeout(() => {
        setShowImageModal(false);
        setUploadSuccess('');
        setImageUrl('');
      }, 1000);
    } catch (err: any) {
      setUploadError(err.message || 'Upload failed');
    } finally {
      setUploading(false);
    }
  }, [editor]);

  const handleFileDrop = useCallback((e: React.DragEvent) => {
    e.preventDefault();
    const file = e.dataTransfer.files[0];
    if (file && file.type.startsWith('image/')) {
      handleFileUpload(file);
    } else {
      setUploadError('Please drop an image file (PNG, JPG, GIF, WebP)');
    }
  }, [handleFileUpload]);

  const handleFileSelect = useCallback((e: React.ChangeEvent<HTMLInputElement>) => {
    const file = e.target.files?.[0];
    if (file) {
      handleFileUpload(file);
    }
  }, [handleFileUpload]);

  if (!editor) {
    return <div className="ee-loading">Loading editor...</div>;
  }

  return (
    <div className={`email-editor ${isFullscreen ? 'fullscreen' : ''}`}>
      {/* Toolbar */}
      <div className="ee-toolbar">
        <div className="ee-toolbar-group">
          <ToolbarButton
            icon={faUndo}
            onClick={() => editor.chain().focus().undo().run()}
            disabled={!editor.can().undo()}
            title="Undo"
          />
          <ToolbarButton
            icon={faRedo}
            onClick={() => editor.chain().focus().redo().run()}
            disabled={!editor.can().redo()}
            title="Redo"
          />
        </div>

        <div className="ee-toolbar-divider" />

        <div className="ee-toolbar-group">
          <select
            className="ee-heading-select"
            value={
              editor.isActive('heading', { level: 1 }) ? 'h1' :
              editor.isActive('heading', { level: 2 }) ? 'h2' :
              editor.isActive('heading', { level: 3 }) ? 'h3' : 'p'
            }
            onChange={(e) => {
              const value = e.target.value;
              if (value === 'p') {
                editor.chain().focus().setParagraph().run();
              } else {
                const level = parseInt(value.replace('h', '')) as 1 | 2 | 3;
                editor.chain().focus().toggleHeading({ level }).run();
              }
            }}
          >
            <option value="p">Paragraph</option>
            <option value="h1">Heading 1</option>
            <option value="h2">Heading 2</option>
            <option value="h3">Heading 3</option>
          </select>
        </div>

        <div className="ee-toolbar-divider" />

        <div className="ee-toolbar-group">
          <ToolbarButton
            icon={faBold}
            onClick={() => editor.chain().focus().toggleBold().run()}
            isActive={editor.isActive('bold')}
            title="Bold (Ctrl+B)"
          />
          <ToolbarButton
            icon={faItalic}
            onClick={() => editor.chain().focus().toggleItalic().run()}
            isActive={editor.isActive('italic')}
            title="Italic (Ctrl+I)"
          />
          <ToolbarButton
            icon={faUnderline}
            onClick={() => editor.chain().focus().toggleUnderline().run()}
            isActive={editor.isActive('underline')}
            title="Underline (Ctrl+U)"
          />
          <ToolbarButton
            icon={faStrikethrough}
            onClick={() => editor.chain().focus().toggleStrike().run()}
            isActive={editor.isActive('strike')}
            title="Strikethrough"
          />
        </div>

        <div className="ee-toolbar-divider" />

        <div className="ee-toolbar-group">
          <ToolbarButton
            icon={faAlignLeft}
            onClick={() => editor.chain().focus().setTextAlign('left').run()}
            isActive={editor.isActive({ textAlign: 'left' })}
            title="Align Left"
          />
          <ToolbarButton
            icon={faAlignCenter}
            onClick={() => editor.chain().focus().setTextAlign('center').run()}
            isActive={editor.isActive({ textAlign: 'center' })}
            title="Align Center"
          />
          <ToolbarButton
            icon={faAlignRight}
            onClick={() => editor.chain().focus().setTextAlign('right').run()}
            isActive={editor.isActive({ textAlign: 'right' })}
            title="Align Right"
          />
        </div>

        <div className="ee-toolbar-divider" />

        <div className="ee-toolbar-group">
          <ToolbarButton
            icon={faListUl}
            onClick={() => editor.chain().focus().toggleBulletList().run()}
            isActive={editor.isActive('bulletList')}
            title="Bullet List"
          />
          <ToolbarButton
            icon={faListOl}
            onClick={() => editor.chain().focus().toggleOrderedList().run()}
            isActive={editor.isActive('orderedList')}
            title="Numbered List"
          />
          <ToolbarButton
            icon={faQuoteRight}
            onClick={() => editor.chain().focus().toggleBlockquote().run()}
            isActive={editor.isActive('blockquote')}
            title="Quote"
          />
        </div>

        <div className="ee-toolbar-divider" />

        <div className="ee-toolbar-group">
          <ToolbarButton
            icon={faLink}
            onClick={openLinkModal}
            isActive={editor.isActive('link')}
            title="Add Link"
          />
          {editor.isActive('link') && (
            <ToolbarButton
              icon={faUnlink}
              onClick={() => editor.chain().focus().unsetLink().run()}
              title="Remove Link"
            />
          )}
          <ToolbarButton
            icon={faImage}
            onClick={() => setShowImageModal(true)}
            title="Add Image"
          />
        </div>

        <div className="ee-toolbar-spacer" />

        <div className="ee-toolbar-group">
          <div className="ee-view-toggle">
            <button
              className={`ee-view-btn ${viewMode === 'edit' ? 'active' : ''}`}
              onClick={() => setViewMode('edit')}
              title="Edit"
            >
              <FontAwesomeIcon icon={faMagic} />
              Edit
            </button>
            <button
              className={`ee-view-btn ${viewMode === 'preview' ? 'active' : ''}`}
              onClick={() => setViewMode('preview')}
              title="Preview"
            >
              <FontAwesomeIcon icon={faEye} />
              Preview
            </button>
            <button
              className={`ee-view-btn ${viewMode === 'code' ? 'active' : ''}`}
              onClick={() => setViewMode('code')}
              title="HTML Code"
            >
              <FontAwesomeIcon icon={faFileCode} />
              Code
            </button>
          </div>
        </div>

        {viewMode === 'preview' && (
          <>
            <div className="ee-toolbar-divider" />
            <div className="ee-toolbar-group">
              <ToolbarButton
                icon={faDesktop}
                onClick={() => setDevicePreview('desktop')}
                isActive={devicePreview === 'desktop'}
                title="Desktop Preview"
              />
              <ToolbarButton
                icon={faMobileAlt}
                onClick={() => setDevicePreview('mobile')}
                isActive={devicePreview === 'mobile'}
                title="Mobile Preview"
              />
            </div>
          </>
        )}

        <div className="ee-toolbar-divider" />

        <ToolbarButton
          icon={isFullscreen ? faCompress : faExpand}
          onClick={() => setIsFullscreen(!isFullscreen)}
          title={isFullscreen ? 'Exit Fullscreen' : 'Fullscreen'}
        />
      </div>

      {/* Editor Content Area */}
      <div className="ee-content-wrapper" style={{ minHeight }}>
        {viewMode === 'edit' && (
          <div className="ee-editor-area">
            <EditorContent editor={editor} className="ee-editor-content" />
          </div>
        )}

        {viewMode === 'preview' && (
          <div className={`ee-preview-area ${devicePreview}`}>
            <div className="ee-preview-frame">
              <div 
                className="ee-preview-content"
                dangerouslySetInnerHTML={{ __html: editor.getHTML() }}
              />
            </div>
          </div>
        )}

        {viewMode === 'code' && (
          <div className="ee-code-area">
            <textarea
              className="ee-code-editor"
              value={editor.getHTML()}
              onChange={(e) => {
                editor.commands.setContent(e.target.value);
                onChange(e.target.value);
              }}
              spellCheck={false}
            />
          </div>
        )}
      </div>

      {/* Link Modal */}
      {showLinkModal && (
        <div className="ee-modal-overlay" onClick={() => setShowLinkModal(false)}>
          <div className="ee-modal" onClick={(e) => e.stopPropagation()}>
            <h3>Insert Link</h3>
            <input
              type="url"
              value={linkUrl}
              onChange={(e) => setLinkUrl(e.target.value)}
              placeholder="https://example.com"
              autoFocus
            />
            <div className="ee-modal-actions">
              <button className="ee-modal-cancel" onClick={() => setShowLinkModal(false)}>
                Cancel
              </button>
              <button className="ee-modal-confirm" onClick={handleSetLink}>
                {linkUrl ? 'Update Link' : 'Remove Link'}
              </button>
            </div>
          </div>
        </div>
      )}

      {/* Image Modal - Upload or URL */}
      {showImageModal && (
        <div className="ee-modal-overlay" onClick={() => { setShowImageModal(false); setUploadError(''); setUploadSuccess(''); }}>
          <div className="ee-modal ee-image-modal" onClick={(e) => e.stopPropagation()}>
            <h3><FontAwesomeIcon icon={faImage} /> Insert Image</h3>

            {/* Tab toggle: Upload vs URL */}
            <div className="ee-image-tabs">
              <button
                className={`ee-image-tab ${imageUploadMode === 'upload' ? 'active' : ''}`}
                onClick={() => setImageUploadMode('upload')}
              >
                <FontAwesomeIcon icon={faUpload} /> Upload
              </button>
              <button
                className={`ee-image-tab ${imageUploadMode === 'url' ? 'active' : ''}`}
                onClick={() => setImageUploadMode('url')}
              >
                <FontAwesomeIcon icon={faLink} /> From URL
              </button>
            </div>

            {imageUploadMode === 'upload' ? (
              <div
                className={`ee-upload-zone ${uploading ? 'uploading' : ''}`}
                onDragOver={(e) => e.preventDefault()}
                onDrop={handleFileDrop}
                onClick={() => !uploading && fileInputRef.current?.click()}
              >
                <input
                  ref={fileInputRef}
                  type="file"
                  accept="image/png,image/jpeg,image/gif,image/webp"
                  onChange={handleFileSelect}
                  style={{ display: 'none' }}
                />
                {uploading ? (
                  <div className="ee-upload-status">
                    <FontAwesomeIcon icon={faSpinner} spin />
                    <span>Uploading to CDN...</span>
                  </div>
                ) : uploadSuccess ? (
                  <div className="ee-upload-status ee-upload-success">
                    <FontAwesomeIcon icon={faCheckCircle} />
                    <span>{uploadSuccess}</span>
                  </div>
                ) : (
                  <>
                    <FontAwesomeIcon icon={faUpload} className="ee-upload-icon" />
                    <p>Click or drag & drop an image</p>
                    <span className="ee-upload-hint">PNG, JPG, GIF, WebP — Max 10MB</span>
                  </>
                )}
              </div>
            ) : (
              <input
                type="url"
                value={imageUrl}
                onChange={(e) => setImageUrl(e.target.value)}
                placeholder="https://example.com/image.jpg"
                autoFocus
              />
            )}

            {uploadError && (
              <div className="ee-upload-error">
                <FontAwesomeIcon icon={faExclamationTriangle} /> {uploadError}
              </div>
            )}

            <div className="ee-modal-actions">
              <button className="ee-modal-cancel" onClick={() => { setShowImageModal(false); setUploadError(''); setUploadSuccess(''); }}>
                Cancel
              </button>
              {imageUploadMode === 'url' && (
                <button className="ee-modal-confirm" onClick={handleAddImage} disabled={!imageUrl}>
                  Insert Image
                </button>
              )}
            </div>
          </div>
        </div>
      )}
    </div>
  );
};

export default EmailEditor;
