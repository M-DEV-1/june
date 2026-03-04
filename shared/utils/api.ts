export const validateApiKey = async (key: string) => {
	try {
		const response = await fetch(`https://generativelanguage.googleapis.com/v1beta/models?key=${key}`);
		return response.ok;
	} catch (e) {
		return false;
	}
};
