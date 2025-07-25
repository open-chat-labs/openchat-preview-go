# OpenChat preview proxy

This is a tiny Go server that exists to avoid CORS problems when rendering link previews.

It will receive the url of the link that we want to preview, fetch the associated page, extract and return the OG meta data required to render the preview and return it to the front end.
