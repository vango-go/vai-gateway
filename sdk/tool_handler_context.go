package vai

import "context"

type toolExecutionClientContextKey struct{}

func contextWithToolExecutionClient(ctx context.Context, client *Client) context.Context {
	if ctx == nil {
		ctx = context.Background()
	}
	if client == nil {
		return ctx
	}
	return context.WithValue(ctx, toolExecutionClientContextKey{}, client)
}

func toolExecutionClientFromContext(ctx context.Context) *Client {
	if ctx == nil {
		return nil
	}
	client, _ := ctx.Value(toolExecutionClientContextKey{}).(*Client)
	return client
}
